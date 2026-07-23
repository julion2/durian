package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/julion2/durian/cli/internal/oauth"
	"github.com/spf13/cobra"
)

var authVerifyGraphCmd = &cobra.Command{
	Use:               "verify-graph <account>",
	Short:             "Check that a Microsoft Graph token can be minted and used",
	Long:              "Mint a Microsoft Graph access token from the stored refresh token and call Graph /me to confirm the account is consented for the Graph scopes. Run after 'durian auth login' re-consents the new scopes.",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runAuthVerifyGraph,
}

func runAuthVerifyGraph(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	account, err := cfg.GetAccountByIdentifier(args[0])
	if err != nil {
		return fmt.Errorf("account not found: %s\nAvailable accounts: %s", args[0], cfg.ListAccountIdentifiers())
	}
	if account.OAuth == nil || account.OAuth.Provider != "microsoft" {
		return fmt.Errorf("verify-graph only applies to Microsoft OAuth accounts")
	}

	authEmail := account.GetAuthEmail()
	token, err := oauth.GetGraphToken(authEmail, account.OAuth.ClientID, account.OAuth.ClientSecret, account.OAuth.Tenant)
	if err != nil {
		return fmt.Errorf("could not mint Graph token (re-consent may be required: durian auth login %s): %w",
			account.GetAliasOrName(), err)
	}

	// Diagnostic: decode (without verifying) the token claims so we can tell a
	// correct token (right audience/scopes/user) from a mailbox/tenant problem.
	printTokenClaims(token.AccessToken)

	// Ping a Mail endpoint (not /me — the profile needs User.Read, which we do
	// not request; our scopes are Mail.ReadWrite / Mail.Send). Listing mail
	// folders exercises exactly the permission the graphbackend will use.
	req, err := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/me/mailFolders?$top=5&$select=displayName", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("graph request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read graph response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graph /me/mailFolders returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var folders struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &folders); err != nil {
		return fmt.Errorf("parse graph response: %w", err)
	}

	names := make([]string, 0, len(folders.Value))
	for _, f := range folders.Value {
		names = append(names, f.DisplayName)
	}
	fmt.Printf("Graph access OK: Mail.ReadWrite works, %d top-level folder(s): %s\n", len(names), strings.Join(names, ", "))
	fmt.Printf("Graph token valid for %s\n", formatDuration(token.ExpiresIn()))
	return nil
}

// printTokenClaims decodes the JWT payload (middle segment) without verifying
// the signature and prints the claims that matter for diagnosing Graph access:
// aud (should be the Graph resource), scp (delegated scopes), tid (tenant),
// and the user identity. Microsoft access tokens for custom apps are JWTs; if
// the token is opaque (not three dot-separated segments) we say so.
func printTokenClaims(accessToken string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		fmt.Fprintln(os.Stderr, "  token: opaque (not a JWT), cannot decode claims")
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "  token: could not decode claims: %v\n", err)
		return
	}
	var claims struct {
		Aud        string `json:"aud"`
		Scp        string `json:"scp"`
		Tid        string `json:"tid"`
		UPN        string `json:"upn"`
		UniqueName string `json:"unique_name"`
		PreferredU string `json:"preferred_username"`
		AppID      string `json:"appid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		fmt.Fprintf(os.Stderr, "  token: could not parse claims: %v\n", err)
		return
	}
	user := claims.UPN
	if user == "" {
		user = claims.PreferredU
	}
	if user == "" {
		user = claims.UniqueName
	}
	fmt.Fprintf(os.Stderr, "  token aud=%s\n", claims.Aud)
	fmt.Fprintf(os.Stderr, "  token scp=%s\n", claims.Scp)
	fmt.Fprintf(os.Stderr, "  token tid=%s user=%s appid=%s\n", claims.Tid, user, claims.AppID)
}
