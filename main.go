package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Config struct {
	MollieAPIKey         string
	CloudreveKey         string
	PublicURL            string
	RedirectTemplate     string
	DBPath               string
	MaxSignatureLifetime int64
	MollieLocale         string
	// AllowedHosts is an optional allow-list (host or host:port, lowercase) of
	// Cloudreve sites this bridge may talk to. Empty means "no allow-list".
	AllowedHosts map[string]bool
}

type App struct {
	cfg   Config
	db    *sql.DB
	locks keyedLock
}

type CloudreveOrder struct {
	Name      string `json:"name"`
	OrderNo   string `json:"order_no"`
	NotifyURL string `json:"notify_url"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
}

type MolliePayment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Amount struct {
		Currency string `json:"currency"`
		Value    string `json:"value"`
	} `json:"amount"`
	Description string `json:"description"`
	Links       struct {
		Checkout struct {
			Href string `json:"href"`
		} `json:"checkout"`
	} `json:"_links"`
}

// httpClient is used for every outbound request (Mollie and Cloudreve). It has
// a timeout so a slow upstream cannot pile up goroutines, and it never follows
// redirects so a callback URL cannot bounce the bridge to another host.
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Mollie payment IDs look like "tr_7UhSN1zuXS".
var molliePaymentIDRe = regexp.MustCompile(`^tr_[A-Za-z0-9]{1,64}$`)

// keyedLock serialises work per key (here: per Cloudreve order number).
type keyedLock struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until the lock for key is acquired and returns the unlock func.
func (k *keyedLock) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*lockEntry)
	}
	e := k.locks[key]
	if e == nil {
		e = &lockEntry{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

func env(name, fallback string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v
}

func parseHostList(v string) map[string]bool {
	out := make(map[string]bool)
	for _, h := range strings.Split(v, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			out[h] = true
		}
	}
	return out
}

func main() {
	maxAge, err := strconv.ParseInt(env("MAX_SIGNATURE_LIFETIME_SECONDS", "900"), 10, 64)
	if err != nil || maxAge < 1 {
		log.Fatal("invalid MAX_SIGNATURE_LIFETIME_SECONDS")
	}

	publicURL := strings.TrimRight(env("PUBLIC_URL", ""), "/")

	cfg := Config{
		MollieAPIKey: env("MOLLIE_API_KEY", ""),
		CloudreveKey: env("CLOUDREVE_COMMUNICATION_KEY", ""),
		PublicURL:    publicURL,
		// By default customers return to the bridge's own thank-you page.
		// Set REDIRECT_URL_TEMPLATE to override; supported placeholders are
		// {cloudreve_site_url} and {order_no}.
		RedirectTemplate:     env("REDIRECT_URL_TEMPLATE", publicURL+"/return?order_no={order_no}"),
		DBPath:               env("DB_PATH", "/data/bridge.db"),
		MaxSignatureLifetime: maxAge,
		MollieLocale:         env("MOLLIE_LOCALE", "nl_NL"),
		AllowedHosts:         parseHostList(env("ALLOWED_CLOUDREVE_HOSTS", "")),
	}

	if cfg.MollieAPIKey == "" || cfg.CloudreveKey == "" || cfg.PublicURL == "" {
		log.Fatal("MOLLIE_API_KEY, CLOUDREVE_COMMUNICATION_KEY and PUBLIC_URL are required")
	}
	if strings.Contains(strings.ToLower(cfg.CloudreveKey), "change-me") {
		log.Fatal("CLOUDREVE_COMMUNICATION_KEY is still the example placeholder; generate one with: openssl rand -hex 32")
	}
	if len(cfg.CloudreveKey) < 32 {
		log.Printf("WARNING: CLOUDREVE_COMMUNICATION_KEY is shorter than 32 characters; generate a stronger one with: openssl rand -hex 32")
	}
	if len(cfg.AllowedHosts) == 0 {
		log.Printf("WARNING: ALLOWED_CLOUDREVE_HOSTS is not set; the bridge will accept any Cloudreve host that presents a valid signature")
	}

	if err := os.MkdirAll(path.Dir(cfg.DBPath), 0755); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite3", cfg.DBPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := &App{cfg: cfg, db: db}
	if err := app.initDB(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /order", app.createOrder)
	mux.HandleFunc("GET /order", app.queryOrder)
	mux.HandleFunc("GET /return", app.returnPage)
	mux.HandleFunc("POST /webhook/mollie", app.mollieWebhook)
	mux.HandleFunc("GET /healthz", app.healthz)

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	log.Printf("bridge build: return-page-v2")
	log.Printf("redirect template: %s", cfg.RedirectTemplate)
	log.Printf("Cloudreve Mollie bridge listening on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

func (a *App) initDB() error {
	_, err := a.db.Exec(`
CREATE TABLE IF NOT EXISTS payments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	order_no TEXT NOT NULL UNIQUE,
	mollie_id TEXT NOT NULL,
	notify_url TEXT NOT NULL,
	cloudreve_site_url TEXT NOT NULL,
	amount INTEGER NOT NULL,
	currency TEXT NOT NULL,
	description TEXT NOT NULL,
	status TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_payments_mollie_id ON payments(mollie_id);

-- Every Mollie payment ever created for an order. An order can have several
-- (a retry after failed/canceled/expired), and a webhook for any of them must
-- still resolve to the order. "notified" makes the Cloudreve callback
-- happen exactly once per paid Mollie payment.
CREATE TABLE IF NOT EXISTS mollie_payments (
	mollie_id TEXT PRIMARY KEY,
	order_no TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT '',
	notified INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_mollie_payments_order ON mollie_payments(order_no);

-- Migrate rows from databases created by earlier versions.
INSERT OR IGNORE INTO mollie_payments (mollie_id, order_no, status)
	SELECT mollie_id, order_no, status FROM payments;
`)
	return err
}

func (a *App) createOrder(w http.ResponseWriter, r *http.Request) {
	if err := a.verifyCreateSignature(r); err != nil {
		log.Printf("create order: signature check failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"code": 50001, "msg": "invalid signature"})
		return
	}

	var order CloudreveOrder
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": "failed to read request"})
		return
	}
	if err := json.Unmarshal(body, &order); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": "invalid JSON"})
		return
	}

	if order.Name == "" || order.OrderNo == "" || order.NotifyURL == "" ||
		order.Amount <= 0 || order.Currency == "" {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": "missing or invalid order fields"})
		return
	}

	siteURL := strings.TrimSpace(r.Header.Get("X-Cr-Site-Url"))
	if siteURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": "missing X-Cr-Site-Url"})
		return
	}
	if err := a.validateCloudreveURLs(siteURL, order.NotifyURL); err != nil {
		log.Printf("create order %s: rejected URLs: %v", order.OrderNo, err)
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": "invalid site or notify URL"})
		return
	}

	currency := strings.ToUpper(order.Currency)

	// Serialise everything below per order, so two concurrent requests for the
	// same order can never create two Mollie payments.
	unlock := a.locks.lock(order.OrderNo)
	defer unlock()

	// If the order already exists, decide based on the state of its latest
	// Mollie payment. Never create a second payment while the first one is
	// open, pending or paid.
	if existing, err := a.getByOrderNo(order.OrderNo); err == nil {
		payment, err := a.getMolliePayment(existing.MollieID)
		if err != nil {
			log.Printf("create order %s: lookup of existing payment failed: %s", order.OrderNo, sanitizeErr(err))
			writeJSON(w, http.StatusOK, map[string]any{"code": 50005, "msg": "Failed to query existing payment"})
			return
		}
		a.updateStatus(existing.MollieID, existing.OrderNo, payment.Status)

		switch payment.Status {
		case "paid":
			writeJSON(w, http.StatusOK, map[string]any{"code": 50007, "msg": "order already paid"})
			return
		case "open":
			if href := payment.Links.Checkout.Href; href != "" {
				writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": href})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"code": 50008, "msg": "payment in progress"})
			return
		case "failed", "canceled", "expired":
			// Terminal and unpaid: safe to start a fresh payment. The old
			// Mollie ID stays in mollie_payments.
		default:
			// pending, authorized, ...: money may still be on its way.
			writeJSON(w, http.StatusOK, map[string]any{"code": 50008, "msg": "payment is being processed"})
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("create order %s: database lookup failed: %v", order.OrderNo, err)
		writeJSON(w, http.StatusOK, map[string]any{"code": 50006, "msg": "Failed to look up order"})
		return
	}

	value, err := smallestUnitToMollieValue(order.Amount, currency)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": err.Error()})
		return
	}

	redirectURL := buildRedirectURL(a.cfg.RedirectTemplate, siteURL, order.OrderNo)
	webhookURL := a.cfg.PublicURL + "/webhook/mollie"

	payload := map[string]any{
		"amount": map[string]string{
			"currency": currency,
			"value":    value,
		},
		"description": truncate(order.Name+" - "+order.OrderNo, 255),
		"redirectUrl": redirectURL,
		"webhookUrl":  webhookURL,
		"locale":      a.cfg.MollieLocale,
		"metadata": map[string]string{
			"cloudreve_order_no": order.OrderNo,
			"cloudreve_site_id":  r.Header.Get("X-Cr-Site-Id"),
		},
	}

	payment, err := a.createMolliePayment(payload)
	if err != nil {
		log.Printf("Mollie create payment failed for order %s: %s", order.OrderNo, sanitizeErr(err))
		writeJSON(w, http.StatusOK, map[string]any{"code": 50005, "msg": "Failed to create payment"})
		return
	}
	if payment.ID == "" || payment.Links.Checkout.Href == "" {
		log.Printf("Mollie returned incomplete payment object for order %s", order.OrderNo)
		writeJSON(w, http.StatusOK, map[string]any{"code": 50005, "msg": "Mollie returned no checkout URL"})
		return
	}

	if err := a.savePayment(order, currency, siteURL, payment); err != nil {
		log.Printf("database save failed for order %s (Mollie payment %s): %v", order.OrderNo, payment.ID, err)
		writeJSON(w, http.StatusOK, map[string]any{"code": 50006, "msg": "Failed to save payment"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"data": payment.Links.Checkout.Href,
	})
}

// savePayment stores the order and registers the new Mollie payment in one
// transaction. Older Mollie payments of the same order are kept.
func (a *App) savePayment(order CloudreveOrder, currency, siteURL string, p MolliePayment) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
INSERT INTO payments
	(order_no, mollie_id, notify_url, cloudreve_site_url, amount, currency, description, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(order_no) DO UPDATE SET
	mollie_id=excluded.mollie_id,
	notify_url=excluded.notify_url,
	cloudreve_site_url=excluded.cloudreve_site_url,
	amount=excluded.amount,
	currency=excluded.currency,
	description=excluded.description,
	status=excluded.status,
	updated_at=CURRENT_TIMESTAMP
`,
		order.OrderNo, p.ID, order.NotifyURL, siteURL, order.Amount,
		currency, order.Name, p.Status,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(`
INSERT OR IGNORE INTO mollie_payments (mollie_id, order_no, status) VALUES (?, ?, ?)`,
		p.ID, order.OrderNo, p.Status,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func (a *App) queryOrder(w http.ResponseWriter, r *http.Request) {
	if err := a.verifyQuerySignature(r); err != nil {
		log.Printf("query order: signature check failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"code": 50001, "msg": "invalid signature"})
		return
	}

	orderNo := r.URL.Query().Get("order_no")
	if orderNo == "" {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50002, "msg": "missing order_no"})
		return
	}

	record, err := a.getByOrderNo(orderNo)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"code": 50004, "msg": "order not found"})
		return
	}

	// "paid" is final. If any Mollie payment of this order is paid, the order
	// is paid, whichever payment happens to be the most recent one.
	if a.orderIsPaid(orderNo) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": "PAID"})
		return
	}

	payment, err := a.getMolliePayment(record.MollieID)
	if err != nil {
		log.Printf("Mollie status lookup failed for order %s: %s", orderNo, sanitizeErr(err))
		writeJSON(w, http.StatusOK, map[string]any{"code": 50005, "msg": "Failed to query payment"})
		return
	}

	a.updateStatus(record.MollieID, record.OrderNo, payment.Status)

	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": strings.ToUpper(payment.Status)})
}

// ---------------------------------------------------------------------------
// Thank-you page shown to the customer after leaving Mollie's checkout.
// ---------------------------------------------------------------------------

type returnView struct {
	Title   string
	Message string
	Button  string
	SiteURL string
	Refresh bool
}

var returnTmpl = template.Must(template.New("return").Parse(`<!doctype html>
<html lang="nl">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="5">{{end}}
<title>{{.Title}}</title>
<style>
	:root { color-scheme: light dark; }
	body {
		margin: 0;
		min-height: 100vh;
		display: flex;
		align-items: center;
		justify-content: center;
		font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
		background: #f4f5f7;
		color: #1c1e21;
	}
	main {
		max-width: 30rem;
		margin: 1rem;
		padding: 2rem;
		border-radius: 12px;
		background: #ffffff;
		box-shadow: 0 2px 12px rgba(0, 0, 0, 0.08);
		text-align: center;
	}
	h1 { margin-top: 0; font-size: 1.5rem; }
	p { line-height: 1.5; }
	.btn {
		display: inline-block;
		margin-top: 1rem;
		padding: 0.75rem 1.5rem;
		border-radius: 8px;
		background: #2563eb;
		color: #ffffff;
		text-decoration: none;
		font-weight: 600;
	}
	@media (prefers-color-scheme: dark) {
		body { background: #121212; color: #e6e6e6; }
		main { background: #1e1e1e; box-shadow: none; }
	}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .SiteURL}}<a class="btn" href="{{.SiteURL}}">{{.Button}}</a>{{end}}
</main>
</body>
</html>
`))

func renderReturn(w http.ResponseWriter, status int, v returnView) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := returnTmpl.Execute(w, v); err != nil {
		log.Printf("return page render failed: %v", err)
	}
}

func (a *App) returnPage(w http.ResponseWriter, r *http.Request) {
	orderNo := r.URL.Query().Get("order_no")
	if orderNo == "" {
		renderReturn(w, http.StatusNotFound, returnView{
			Title:   "Bestelling niet gevonden",
			Message: "Er is geen bestelnummer opgegeven.",
		})
		return
	}

	record, err := a.getByOrderNo(orderNo)
	if err != nil {
		renderReturn(w, http.StatusNotFound, returnView{
			Title:   "Bestelling niet gevonden",
			Message: "Deze bestelling is niet bekend.",
		})
		return
	}

	payment, err := a.getMolliePayment(record.MollieID)
	if err != nil {
		log.Printf("return page: status lookup failed for order %s: %s", orderNo, sanitizeErr(err))
		renderReturn(w, http.StatusBadGateway, returnView{
			Title:   "Status onbekend",
			Message: "We konden de status van je betaling nu niet ophalen. Ververs de pagina over een paar seconden.",
		})
		return
	}

	a.updateStatus(record.MollieID, record.OrderNo, payment.Status)

	switch payment.Status {
	case "paid":
		renderReturn(w, http.StatusOK, returnView{
			Title:   "Betaling gelukt",
			Message: "Bedankt! Je betaling is ontvangen. Log in op de site om je aankoop te gebruiken.",
			Button:  "Naar de site",
			SiteURL: record.CloudreveSiteURL,
		})
	case "failed", "canceled", "expired":
		renderReturn(w, http.StatusOK, returnView{
			Title:   "Betaling niet afgerond",
			Message: "Er is niet betaald. Je kunt het opnieuw proberen op de site.",
			Button:  "Naar de site",
			SiteURL: record.CloudreveSiteURL,
		})
	default:
		// open, pending, authorized: the payment is still being processed.
		renderReturn(w, http.StatusOK, returnView{
			Title:   "Betaling wordt verwerkt",
			Message: "We wachten op de bevestiging van je betaling. Deze pagina wordt automatisch vernieuwd.",
			Refresh: true,
		})
	}
}

func (a *App) mollieWebhook(w http.ResponseWriter, r *http.Request) {
	// Mollie webhooks contain only the payment ID. The endpoint is
	// unauthenticated by design, so it must be cheap for unknown input: we
	// validate the ID and check our own database BEFORE calling the Mollie API,
	// and then take the real status from Mollie instead of trusting the request.
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	values, _ := url.ParseQuery(string(body))
	mollieID := values.Get("id")
	if mollieID == "" {
		// Mollie normally posts form data. Accept JSON too for easier testing.
		var obj struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &obj)
		mollieID = obj.ID
	}
	if !molliePaymentIDRe.MatchString(mollieID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	record, err := a.getByMollieID(mollieID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unknown payment: answer 200 so Mollie does not retry, and do not
			// spend a Mollie API call on it.
			log.Printf("webhook: unknown Mollie payment %s", mollieID)
			w.WriteHeader(http.StatusOK)
			return
		}
		log.Printf("webhook: database lookup failed for %s: %v", mollieID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	payment, err := a.getMolliePayment(mollieID)
	if err != nil {
		log.Printf("webhook: failed to fetch %s: %s", mollieID, sanitizeErr(err))
		http.Error(w, "failed to fetch payment", http.StatusBadGateway)
		return
	}

	a.updateStatus(mollieID, record.OrderNo, payment.Status)

	if payment.Status != "paid" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Notify Cloudreve exactly once per paid payment. claimNotification is an
	// atomic 0 -> 1 flip, so concurrent or repeated webhooks cannot both win.
	claimed, err := a.claimNotification(mollieID)
	if err != nil {
		log.Printf("webhook: database error for %s: %v", mollieID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !claimed {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := a.notifyCloudreve(record.NotifyURL); err != nil {
		log.Printf("webhook: Cloudreve callback failed for order %s: %s", record.OrderNo, sanitizeErr(err))
		a.releaseNotification(mollieID)
		// A 5xx makes Mollie retry the webhook later. Cloudreve's own polling
		// of GET /order can also confirm the payment in the meantime.
		http.Error(w, "callback failed", http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type paymentRecord struct {
	OrderNo          string
	MollieID         string
	NotifyURL        string
	CloudreveSiteURL string
}

func (a *App) getByOrderNo(orderNo string) (paymentRecord, error) {
	var p paymentRecord
	err := a.db.QueryRow(`
SELECT order_no, mollie_id, notify_url, cloudreve_site_url
FROM payments WHERE order_no = ?`, orderNo).
		Scan(&p.OrderNo, &p.MollieID, &p.NotifyURL, &p.CloudreveSiteURL)
	return p, err
}

// getByMollieID resolves any Mollie payment of an order (not only the latest)
// to the order it belongs to.
func (a *App) getByMollieID(id string) (paymentRecord, error) {
	var p paymentRecord
	err := a.db.QueryRow(`
SELECT p.order_no, mp.mollie_id, p.notify_url, p.cloudreve_site_url
FROM mollie_payments mp
JOIN payments p ON p.order_no = mp.order_no
WHERE mp.mollie_id = ?`, id).
		Scan(&p.OrderNo, &p.MollieID, &p.NotifyURL, &p.CloudreveSiteURL)
	return p, err
}

func (a *App) updateStatus(mollieID, orderNo, status string) {
	if _, err := a.db.Exec(
		`UPDATE mollie_payments SET status=?, updated_at=CURRENT_TIMESTAMP WHERE mollie_id=?`,
		status, mollieID); err != nil {
		log.Printf("database: status update failed for Mollie payment %s: %v", mollieID, err)
	}
	// Only the latest Mollie payment of an order is mirrored on the order row.
	if _, err := a.db.Exec(
		`UPDATE payments SET status=?, updated_at=CURRENT_TIMESTAMP WHERE order_no=? AND mollie_id=?`,
		status, orderNo, mollieID); err != nil {
		log.Printf("database: status update failed for order %s: %v", orderNo, err)
	}
}

func (a *App) orderIsPaid(orderNo string) bool {
	var n int
	if err := a.db.QueryRow(
		`SELECT COUNT(1) FROM mollie_payments WHERE order_no=? AND status='paid'`, orderNo).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (a *App) claimNotification(mollieID string) (bool, error) {
	res, err := a.db.Exec(
		`UPDATE mollie_payments SET notified=1, updated_at=CURRENT_TIMESTAMP WHERE mollie_id=? AND notified=0`,
		mollieID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (a *App) releaseNotification(mollieID string) {
	if _, err := a.db.Exec(
		`UPDATE mollie_payments SET notified=0, updated_at=CURRENT_TIMESTAMP WHERE mollie_id=?`,
		mollieID); err != nil {
		log.Printf("database: could not release notification claim for %s: %v", mollieID, err)
	}
}

func (a *App) createMolliePayment(payload map[string]any) (MolliePayment, error) {
	var out MolliePayment

	b, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.mollie.com/v2/payments", bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.MollieAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("Mollie HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("invalid Mollie response: %w", err)
	}
	return out, nil
}

func (a *App) getMolliePayment(id string) (MolliePayment, error) {
	var out MolliePayment

	req, err := http.NewRequest(http.MethodGet, "https://api.mollie.com/v2/payments/"+url.PathEscape(id), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.MollieAPIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("Mollie HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (a *App) notifyCloudreve(notifyURL string) error {
	req, err := http.NewRequest(http.MethodGet, notifyURL, nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// The body is drained (bounded) but deliberately not logged: it comes from
	// a remote host and may contain data that does not belong in the logs.
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
	log.Printf("Cloudreve callback: HTTP %d (%d bytes)", resp.StatusCode, n)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudreve callback HTTP %d", resp.StatusCode)
	}
	return nil
}

// validateCloudreveURLs makes sure the callback target is the same site that
// created the order, and, when ALLOWED_CLOUDREVE_HOSTS is set, that this site
// is one we expect. This limits what the bridge can be pointed at (SSRF).
func (a *App) validateCloudreveURLs(siteURL, notifyURL string) error {
	site, err := parseHTTPURL(siteURL)
	if err != nil {
		return fmt.Errorf("site URL: %w", err)
	}
	notify, err := parseHTTPURL(notifyURL)
	if err != nil {
		return fmt.Errorf("notify URL: %w", err)
	}
	if !strings.EqualFold(site.Host, notify.Host) {
		return errors.New("notify URL host does not match site URL host")
	}
	if len(a.cfg.AllowedHosts) > 0 && !a.cfg.AllowedHosts[strings.ToLower(site.Host)] {
		return fmt.Errorf("host %q is not in ALLOWED_CLOUDREVE_HOSTS", site.Host)
	}
	return nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("scheme must be http or https")
	}
	if u.Hostname() == "" {
		return nil, errors.New("missing host")
	}
	if u.User != nil {
		return nil, errors.New("credentials in URL are not allowed")
	}
	return u, nil
}

// sanitizeErr strips the request URL from transport errors, because callback
// URLs can carry signatures that should not end up in the logs.
func sanitizeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

func (a *App) verifyCreateSignature(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer Cr ") {
		return errors.New("invalid Authorization header format")
	}

	sig, ts, err := splitSignature(strings.TrimPrefix(auth, "Bearer Cr "))
	if err != nil {
		return err
	}

	if err := validateTimestamp(ts, a.cfg.MaxSignatureLifetime); err != nil {
		return err
	}

	headers := make([]string, 0)
	for k, vals := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-cr-") {
			for _, v := range vals {
				headers = append(headers, fmt.Sprintf("%s=%s", k, v))
			}
		}
	}
	sort.Strings(headers)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	p := r.URL.Path
	if p == "" {
		p = "/"
	}

	signContent, err := json.Marshal(struct {
		Path   string `json:"Path"`
		Header string `json:"Header"`
		Body   string `json:"Body"`
	}{p, strings.Join(headers, "&"), string(body)})
	if err != nil {
		return err
	}

	expected := hmacSignature(a.cfg.CloudreveKey, string(signContent)+":"+ts)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("invalid signature")
	}
	return nil
}

// verifyQuerySignature checks the signature of GET /order requests. Cloudreve
// signs only the URL path (without query string) followed by ":" and the
// expiry timestamp.
func (a *App) verifyQuerySignature(r *http.Request) error {
	raw := r.URL.Query().Get("sign")
	if raw == "" {
		return errors.New("missing sign")
	}

	sig, ts, err := splitSignature(raw)
	if err != nil {
		return err
	}

	if err := validateTimestamp(ts, a.cfg.MaxSignatureLifetime); err != nil {
		return err
	}

	p := r.URL.Path
	if p == "" {
		p = "/"
	}

	expected := hmacSignature(a.cfg.CloudreveKey, p+":"+ts)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("invalid signature")
	}
	return nil
}

func splitSignature(v string) (string, string, error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("invalid signature format")
	}
	return parts[0], parts[1], nil
}

func validateTimestamp(ts string, maxFuture int64) error {
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("invalid timestamp")
	}
	now := time.Now().Unix()
	if now > t {
		return errors.New("signature expired")
	}
	if t-now > maxFuture {
		return errors.New("signature timestamp too far in the future")
	}
	return nil
}

func hmacSignature(key, content string) string {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write([]byte(content))
	return base64.URLEncoding.EncodeToString(h.Sum(nil))
}

func smallestUnitToMollieValue(amount int64, currency string) (string, error) {
	// Cloudreve sends the smallest currency unit. For currencies with 2
	// decimals, 1000 => "10.00". Mollie supports currencies with 0 or 2 decimals
	// in this bridge.
	zeroDecimal := map[string]bool{
		"JPY": true, "KRW": true, "VND": true, "CLP": true,
		"XAF": true, "XOF": true, "XPF": true, "PYG": true,
		"RWF": true, "UGX": true,
	}

	if amount < 1 {
		return "", errors.New("amount must be positive")
	}

	if zeroDecimal[currency] {
		return strconv.FormatInt(amount, 10), nil
	}

	return fmt.Sprintf("%d.%02d", amount/100, amount%100), nil
}

func buildRedirectURL(template, siteURL, orderNo string) string {
	return strings.NewReplacer(
		"{cloudreve_site_url}", strings.TrimRight(siteURL, "/"),
		"{order_no}", url.QueryEscape(orderNo),
	).Replace(template)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// loggingMiddleware logs the method and path only. It deliberately leaves out
// the query string, because GET /order carries a reusable signature in it.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
