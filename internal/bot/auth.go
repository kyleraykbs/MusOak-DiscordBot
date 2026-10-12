package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/config"
	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/state"
)

// accounts owns every musoak session the bot holds: the placeholder account's
// own, and each linked user's. Sessions last thirty days; when one has expired
// the stored credentials buy a new one, so nobody is asked to link twice.
type accounts struct {
	serverURL   string
	username    string
	password    string
	displayName string
	store       *state.Store
	logger      *slog.Logger

	mu     sync.Mutex
	token  string // the placeholder's current session
	userID string // the placeholder's own account id, once known
}

func newAccounts(cfg *config.Config, store *state.Store, logger *slog.Logger) *accounts {
	return &accounts{
		serverURL:   cfg.ServerURL,
		username:    cfg.Account.Username,
		password:    cfg.Account.Password,
		displayName: cfg.Account.DisplayName,
		store:       store,
		logger:      logger,
	}
}

// login exchanges the placeholder credentials for a session token.
func (a *accounts) login(ctx context.Context) (string, error) {
	auth, err := client.New(a.serverURL).Login(ctx, a.username, a.password)
	if err != nil {
		return "", fmt.Errorf("login as %q: %w", a.username, err)
	}
	a.mu.Lock()
	a.token = auth.Token
	a.userID = auth.User.ID
	a.mu.Unlock()
	return auth.Token, nil
}

// placeholderID is the account id of the placeholder, once a login has named
// it. Empty before that.
func (a *accounts) placeholderID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.userID
}

func (a *accounts) currentToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

// withPlaceholder runs fn as the placeholder account, logging in once more
// when the session turns out to be dead.
func (a *accounts) withPlaceholder(ctx context.Context, fn func(*client.Client) error) error {
	token := a.currentToken()
	if token == "" {
		var err error
		if token, err = a.login(ctx); err != nil {
			return err
		}
	}
	err := fn(client.New(a.serverURL, client.WithToken(token)))
	if err == nil || !client.IsUnauthorized(err) {
		return err
	}
	if token, err = a.login(ctx); err != nil {
		return fmt.Errorf("session expired: %w", err)
	}
	return fn(client.New(a.serverURL, client.WithToken(token)))
}

// withUser runs fn as the linked account. It reports whether the Discord user
// is linked at all; an unlinked one is not an error, the caller decides.
func (a *accounts) withUser(ctx context.Context, discordID string, fn func(*client.Client) error) (bool, error) {
	link, ok, err := a.store.LinkOf(ctx, discordID)
	if err != nil || !ok {
		return ok, err
	}
	run := func(token string) error { return fn(client.New(a.serverURL, client.WithToken(token))) }

	err = run(link.Token)
	if err == nil || !client.IsUnauthorized(err) {
		return true, err
	}
	auth, loginErr := client.New(a.serverURL).Login(ctx, link.Username, link.Password)
	if loginErr != nil {
		return true, fmt.Errorf("your MusOak session expired and re-login failed: %w", loginErr)
	}
	if storeErr := a.store.SetToken(ctx, discordID, auth.Token); storeErr != nil {
		a.logger.Warn("link: could not save the refreshed token", "discord", discordID, "error", storeErr)
	}
	return true, run(auth.Token)
}

// requester runs fn as the Discord user when they are linked, and as the
// placeholder account when they are not — which is what makes an unlinked
// user's songs land under "Discord Bot".
func (a *accounts) requester(ctx context.Context, discordID string, fn func(*client.Client) error) (linked bool, err error) {
	if linked, err = a.withUser(ctx, discordID, fn); err != nil || linked {
		return linked, err
	}
	return false, a.withPlaceholder(ctx, fn)
}

// setDisplayName pins the placeholder account's display name — the name the
// room shows for every unlinked user's queue items.
func (a *accounts) setDisplayName(ctx context.Context) error {
	if a.displayName == "" {
		return nil
	}
	return a.withPlaceholder(ctx, func(c *client.Client) error {
		body, err := json.Marshal(map[string]string{"displayName": a.displayName})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
			c.BaseURL()+"/api/v1/me", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("PATCH /me: %s", resp.Status)
		}
		return nil
	})
}
