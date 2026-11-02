package push

import (
	"encoding/json"
	"log"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Notice is one notification. It never carries terminal output.
type Notice struct {
	Address string // terminal address; also the notification tag and the URL to open
	Title   string
	Body    string
	Urgent  bool
	URL     string // page to open on click; default /t/<Address>
}

// Payload encodes n in the Angular service worker notification format.
func Payload(n Notice) []byte {
	url := n.URL
	if url == "" {
		url = "/t/" + n.Address
	}
	b, _ := json.Marshal(map[string]any{"notification": map[string]any{
		"title": n.Title,
		"body":  n.Body,
		"tag":   n.Address,
		"icon":  "/icons/icon-192.png",
		"data": map[string]any{"onActionClick": map[string]any{
			"default": map[string]string{"operation": "navigateLastFocusedOrOpen", "url": url},
		}},
	}})
	return b
}

// Sender pushes notices to every stored subscription.
type Sender struct {
	Store   *Store
	VAPID   VAPID
	Subject string
	Client  webpush.HTTPClient // nil: default client
}

// Notify sends n to all subscriptions, deleting those the push service reports gone (404/410).
// It logs only counts and status codes.
func (s *Sender) Notify(n Notice) {
	opts := &webpush.Options{
		HTTPClient:      s.Client,
		Subscriber:      s.Subject,
		TTL:             3600,
		VAPIDPublicKey:  s.VAPID.Public,
		VAPIDPrivateKey: s.VAPID.Private,
	}
	if n.Urgent {
		opts.Urgency = webpush.UrgencyHigh
	}
	payload := Payload(n)
	sent, failed, removed := 0, 0, 0
	for _, sub := range s.Store.List() {
		sub := sub
		resp, err := webpush.SendNotification(payload, &sub, opts)
		if err != nil {
			failed++
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			if s.Store.Delete(sub.Endpoint) == nil {
				removed++
			}
		case resp.StatusCode >= 300:
			failed++
			log.Printf("push: service answered %d", resp.StatusCode)
		default:
			sent++
		}
	}
	if sent+failed+removed > 0 {
		log.Printf("push: sent=%d failed=%d removed=%d", sent, failed, removed)
	}
}
