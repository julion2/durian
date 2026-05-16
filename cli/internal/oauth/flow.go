package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const (
	// CallbackPath is the path for the OAuth callback
	CallbackPath = "/callback"
	// DefaultTimeout is the default timeout for waiting for the OAuth callback
	DefaultTimeout = 5 * time.Minute
)

// FlowResult contains the result of an OAuth flow
type FlowResult struct {
	Code  string
	State string
	Error string
}

// StartFlow initiates the OAuth authorization flow
// It opens the browser to the authorization URL and waits for the callback
func StartFlow(provider *Provider, clientID, redirectURI string, pkce *PKCE) (*FlowResult, error) {
	// Generate random state for CSRF protection
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("failed to generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	// Build authorization URL
	authURL := provider.AuthorizationURL(clientID, redirectURI, state, pkce)

	// Start callback server
	resultChan := make(chan *FlowResult, 1)
	errChan := make(chan error, 1)

	server, port, err := startCallbackServer(state, resultChan, errChan)
	if err != nil {
		return nil, err
	}
	defer server.Shutdown(context.Background())

	// Verify callback server is on the expected port
	if fmt.Sprintf("http://localhost:%d%s", port, CallbackPath) != redirectURI {
		server.Shutdown(context.Background())
		return nil, fmt.Errorf("callback server port mismatch")
	}

	// Open browser (user-facing CLI prompts go to stderr per CLI convention)
	fmt.Fprintln(os.Stderr, "Opening browser for authorization...")
	fmt.Fprintf(os.Stderr, "If browser doesn't open, visit:\n%s\n\n", authURL)

	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not open browser automatically: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "Waiting for authorization (timeout: %v)...\n", DefaultTimeout)

	// Wait for callback or timeout
	select {
	case result := <-resultChan:
		if result.Error != "" {
			return nil, fmt.Errorf("authorization error: %s", result.Error)
		}
		return result, nil
	case err := <-errChan:
		return nil, err
	case <-time.After(DefaultTimeout):
		return nil, fmt.Errorf("authorization timed out after %v", DefaultTimeout)
	}
}

// startCallbackServer starts an HTTP server to receive the OAuth callback
func startCallbackServer(expectedState string, resultChan chan *FlowResult, errChan chan error) (*http.Server, int, error) {
	const port = 8080
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, 0, fmt.Errorf("OAuth callback port %d is in use, please free it and retry: %w", port, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		// Check for error
		if errMsg := query.Get("error"); errMsg != "" {
			errDesc := query.Get("error_description")
			// HTML-escape provider-supplied values before embedding them in the
			// response page — a hostile OAuth provider could otherwise inject
			// script tags that run in the browser session sharing localhost.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Authorization Failed</title></head>
<body>
<h1>Authorization Failed</h1>
<p>Error: %s</p>
<p>%s</p>
<p>You can close this window.</p>
</body>
</html>`, html.EscapeString(errMsg), html.EscapeString(errDesc))
			resultChan <- &FlowResult{Error: fmt.Sprintf("%s: %s", errMsg, errDesc)}
			return
		}

		// Verify state
		state := query.Get("state")
		if state != expectedState {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Authorization Failed</title></head>
<body>
<h1>Authorization Failed</h1>
<p>Invalid state parameter. This may be a CSRF attack.</p>
<p>You can close this window.</p>
</body>
</html>`)
			errChan <- fmt.Errorf("state mismatch: possible CSRF attack")
			return
		}

		// Get authorization code
		code := query.Get("code")
		if code == "" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Authorization Failed</title></head>
<body>
<h1>Authorization Failed</h1>
<p>No authorization code received.</p>
<p>You can close this window.</p>
</body>
</html>`)
			errChan <- fmt.Errorf("no authorization code in callback")
			return
		}

		// Success!
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Authorization Successful</title></head>
<body>
<h1>Authorization Successful!</h1>
<p>You can close this window and return to the terminal.</p>
</body>
</html>`)

		resultChan <- &FlowResult{
			Code:  code,
			State: state,
		}
	})

	server := &http.Server{Handler: mux}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("callback server error: %w", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "OAuth callback server listening on http://localhost:%d%s\n", port, CallbackPath)

	return server, port, nil
}

// openBrowser opens the default browser to the given URL
func openBrowser(url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return cmd.Start()
}

// Authenticate performs the full OAuth authentication flow
// Returns the token ready to be stored
// clientSecret is required for Google, optional for Microsoft
func Authenticate(provider *Provider, clientID, clientSecret, email string) (*Token, error) {
	clientID, clientSecret = provider.ResolveCredentials(clientID, clientSecret)

	// Generate PKCE
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("failed to generate PKCE: %w", err)
	}

	redirectURI := "http://localhost:8080" + CallbackPath

	result, err := StartFlow(provider, clientID, redirectURI, pkce)
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(os.Stderr, "Exchanging authorization code for tokens...")

	// Exchange code for tokens
	token, err := ExchangeCode(provider, clientID, clientSecret, redirectURI, result.Code, pkce.Verifier)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}

	return token, nil
}
