package main

import (
	"context"
	"encoding/hex"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const sessionCookieName = "nostr_session"
const sessionMaxAge = 24 * time.Hour

// htmlLoginHandler shows the login page (GET) or processes login (POST)
func htmlLoginHandler(w http.ResponseWriter, r *http.Request) {
	// Handle POST - delegate to submit handler
	if r.Method == http.MethodPost {
		htmlLoginSubmitHandler(w, r)
		return
	}

	// Check if already logged in
	session := getSessionFromRequest(r)
	if session != nil && session.Connected {
		http.Redirect(w, r, "/html/timeline?kinds=1&limit=20", http.StatusSeeOther)
		return
	}

	data := struct {
		Title   string
		Error   string
		Success string
	}{
		Title: "Login with Nostr Connect",
	}

	// Check for error/success messages in query params
	data.Error = r.URL.Query().Get("error")
	data.Success = r.URL.Query().Get("success")

	renderLoginPage(w, data)
}

// htmlLoginSubmitHandler processes the bunker URL submission
func htmlLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/html/login", http.StatusSeeOther)
		return
	}

	bunkerURL := strings.TrimSpace(r.FormValue("bunker_url"))
	if bunkerURL == "" {
		http.Redirect(w, r, "/html/login?error=Please+enter+a+bunker+URL", http.StatusSeeOther)
		return
	}

	// Parse bunker URL
	session, err := ParseBunkerURL(bunkerURL)
	if err != nil {
		log.Printf("Failed to parse bunker URL: %v", err)
		http.Redirect(w, r, "/html/login?error="+escapeURLParam(err.Error()), http.StatusSeeOther)
		return
	}

	// Attempt to connect (with timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log.Printf("Connecting to bunker at %v...", session.Relays)
	if err := session.Connect(ctx); err != nil {
		log.Printf("Failed to connect to bunker: %v", err)
		http.Redirect(w, r, "/html/login?error="+escapeURLParam("Connection failed: "+err.Error()), http.StatusSeeOther)
		return
	}

	// Store session
	bunkerSessions.Set(session)

	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session.ID,
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	log.Printf("User logged in: %s", hex.EncodeToString(session.UserPubKey))
	http.Redirect(w, r, "/html/timeline?kinds=1&limit=20&success=Logged+in+successfully", http.StatusSeeOther)
}

// htmlLogoutHandler logs out the user
func htmlLogoutHandler(w http.ResponseWriter, r *http.Request) {
	session := getSessionFromRequest(r)
	if session != nil {
		bunkerSessions.Delete(session.ID)
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	http.Redirect(w, r, "/html/login?success=Logged+out", http.StatusSeeOther)
}

// htmlPostNoteHandler handles note posting via POST form
func htmlPostNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/html/timeline?kinds=1&limit=20", http.StatusSeeOther)
		return
	}

	session := getSessionFromRequest(r)
	if session == nil || !session.Connected {
		http.Redirect(w, r, "/html/login?error=Please+login+first", http.StatusSeeOther)
		return
	}

	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		http.Redirect(w, r, "/html/timeline?kinds=1&limit=20&error=Note+content+is+required", http.StatusSeeOther)
		return
	}

	// Create unsigned event
	event := UnsignedEvent{
		Kind:      1,
		Content:   content,
		Tags:      [][]string{},
		CreatedAt: time.Now().Unix(),
	}

	// Sign via bunker
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	signedEvent, err := session.SignEvent(ctx, event)
	if err != nil {
		log.Printf("Failed to sign event: %v", err)
		http.Redirect(w, r, "/html/timeline?kinds=1&limit=20&error="+escapeURLParam("Failed to sign: "+err.Error()), http.StatusSeeOther)
		return
	}

	// Publish to relays
	relays := []string{
		"wss://relay.damus.io",
		"wss://relay.nostr.band",
		"wss://relay.primal.net",
		"wss://nos.lol",
	}

	publishEvent(ctx, relays, signedEvent)

	log.Printf("Published note: %s", signedEvent.ID)
	http.Redirect(w, r, "/html/timeline?kinds=1&limit=20&success=Note+published", http.StatusSeeOther)
}

// getSessionFromRequest retrieves the bunker session from the request cookie
func getSessionFromRequest(r *http.Request) *BunkerSession {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	return bunkerSessions.Get(cookie.Value)
}

func escapeURLParam(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "+"), ":", "%3A")
}

// publishEvent publishes a signed event to relays
func publishEvent(ctx context.Context, relays []string, event *Event) {
	for _, relay := range relays {
		go func(relayURL string) {
			if err := publishToRelay(ctx, relayURL, event); err != nil {
				log.Printf("Failed to publish to %s: %v", relayURL, err)
			} else {
				log.Printf("Published to %s", relayURL)
			}
		}(relay)
	}
	// Give relays a moment to receive
	time.Sleep(500 * time.Millisecond)
}

func publishToRelay(ctx context.Context, relayURL string, event *Event) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, relayURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := []interface{}{"EVENT", event}
	return conn.WriteJSON(req)
}

var htmlLoginTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{.Title}} - Nostr Hypermedia</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
      line-height: 1.6;
      color: #333;
      background: #f5f5f5;
      padding: 20px;
    }
    .container {
      max-width: 600px;
      margin: 0 auto;
      background: white;
      border-radius: 8px;
      box-shadow: 0 2px 8px rgba(0,0,0,0.1);
      overflow: hidden;
    }
    header {
      background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
      color: white;
      padding: 30px;
      text-align: center;
    }
    header h1 { font-size: 28px; margin-bottom: 8px; }
    .subtitle { opacity: 0.9; font-size: 14px; }
    nav {
      padding: 15px;
      background: #f8f9fa;
      border-bottom: 1px solid #dee2e6;
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
    }
    nav a {
      padding: 8px 16px;
      background: #667eea;
      color: white;
      text-decoration: none;
      border-radius: 4px;
      font-size: 14px;
      transition: background 0.2s;
    }
    nav a:hover { background: #5568d3; }
    main { padding: 30px; }
    .alert {
      padding: 12px 16px;
      border-radius: 4px;
      margin-bottom: 20px;
      font-size: 14px;
    }
    .alert-error {
      background: #fee2e2;
      color: #dc2626;
      border: 1px solid #fecaca;
    }
    .alert-success {
      background: #dcfce7;
      color: #16a34a;
      border: 1px solid #bbf7d0;
    }
    .login-form {
      background: #f8f9fa;
      padding: 24px;
      border-radius: 8px;
      border: 1px solid #dee2e6;
    }
    .form-group {
      margin-bottom: 20px;
    }
    .form-group label {
      display: block;
      font-weight: 600;
      margin-bottom: 8px;
      color: #333;
    }
    .form-group input {
      width: 100%;
      padding: 12px;
      border: 1px solid #ced4da;
      border-radius: 4px;
      font-size: 14px;
      font-family: monospace;
    }
    .form-group input:focus {
      outline: none;
      border-color: #667eea;
      box-shadow: 0 0 0 3px rgba(102, 126, 234, 0.2);
    }
    .form-help {
      font-size: 12px;
      color: #666;
      margin-top: 8px;
    }
    .submit-btn {
      width: 100%;
      padding: 14px;
      background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
      color: white;
      border: none;
      border-radius: 4px;
      font-size: 16px;
      font-weight: 600;
      cursor: pointer;
      transition: transform 0.1s, box-shadow 0.2s;
    }
    .submit-btn:hover {
      transform: translateY(-1px);
      box-shadow: 0 4px 12px rgba(102, 126, 234, 0.4);
    }
    .submit-btn:active {
      transform: translateY(0);
    }
    .info-section {
      margin-top: 30px;
      padding-top: 20px;
      border-top: 1px solid #dee2e6;
    }
    .info-section h3 {
      font-size: 16px;
      margin-bottom: 12px;
      color: #555;
    }
    .info-section p {
      font-size: 14px;
      color: #666;
      margin-bottom: 12px;
    }
    .info-section code {
      background: #e9ecef;
      padding: 2px 6px;
      border-radius: 3px;
      font-size: 13px;
    }
    .info-section ul {
      margin-left: 20px;
      font-size: 14px;
      color: #666;
    }
    .info-section li {
      margin-bottom: 8px;
    }
    .info-section a {
      color: #667eea;
    }
    footer {
      text-align: center;
      padding: 20px;
      background: #f8f9fa;
      color: #666;
      font-size: 13px;
      border-top: 1px solid #dee2e6;
    }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <h1>{{.Title}}</h1>
      <p class="subtitle">Zero-Trust Remote Signing (NIP-46)</p>
    </header>

    <nav>
      <a href="/html/timeline?kinds=1&limit=20&fast=1">Timeline</a>
      <a href="/">JS Client</a>
    </nav>

    <main>
      {{if .Error}}
      <div class="alert alert-error">{{.Error}}</div>
      {{end}}
      {{if .Success}}
      <div class="alert alert-success">{{.Success}}</div>
      {{end}}

      <form class="login-form" method="POST" action="/html/login">
        <div class="form-group">
          <label for="bunker_url">Bunker URL</label>
          <input type="text" id="bunker_url" name="bunker_url"
                 placeholder="bunker://pubkey?relay=wss://...&secret=..."
                 required autocomplete="off">
          <p class="form-help">
            Paste your bunker:// URL from your Nostr signer app (nsec.app, Amber, etc.)
          </p>
        </div>
        <button type="submit" class="submit-btn">Connect</button>
      </form>

      <div class="info-section">
        <h3>How it works</h3>
        <p>
          This login uses <strong>NIP-46 (Nostr Connect)</strong> - your private key never leaves your signer app.
          The server only sees your public key and cannot sign events without your approval.
        </p>
        <h3>Supported signers</h3>
        <ul>
          <li><a href="https://nsec.app" target="_blank">nsec.app</a> - Web-based remote signer</li>
          <li><a href="https://github.com/greenart7c3/Amber" target="_blank">Amber</a> - Android signer</li>
          <li>Any NIP-46 compatible bunker</li>
        </ul>
      </div>
    </main>

    <footer>
      <p>Pure HTML hypermedia - no JavaScript required</p>
    </footer>
  </div>
</body>
</html>
`

func renderLoginPage(w http.ResponseWriter, data interface{}) {
	tmpl, err := template.New("login").Parse(htmlLoginTemplate)
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, data)
}
