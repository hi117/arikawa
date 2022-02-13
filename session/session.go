// Package session abstracts around the REST API and the Gateway, managing both
// at once. It offers a handler interface similar to that in discordgo for
// Gateway events.
package session

import (
	"context"
	"sync"

	"github.com/pkg/errors"

	"github.com/hi117/arikawa/v3/api"
	"github.com/hi117/arikawa/v3/gateway"
	"github.com/hi117/arikawa/v3/utils/handler"
	"github.com/hi117/arikawa/v3/utils/json/option"
	"github.com/hi117/arikawa/v3/utils/ws/ophandler"
)

// ErrMFA is returned if the account requires a 2FA code to log in.
var ErrMFA = errors.New("account has 2FA enabled")

// Session manages both the API and Gateway. As such, Session inherits all of
// API's methods, as well has the Handler used for Gateway.
type Session struct {
	*api.Client
	*handler.Handler

	// internal state to not be copied around.
	state *sessionState
}

type sessionState struct {
	sync.Mutex
	id      gateway.Identifier
	gateway *gateway.Gateway

	ctx    context.Context
	cancel context.CancelFunc
	doneCh <-chan struct{}
}

// NewWithIntents is similar to New but adds the given intents in during
// construction.
func NewWithIntents(token string, intents ...gateway.Intents) *Session {
	var allIntent gateway.Intents
	for _, intent := range intents {
		allIntent |= intent
	}

	id := gateway.DefaultIdentifier(token)
	id.Intents = option.NewUint(uint(allIntent))

	return NewWithIdentifier(id)
}

// New creates a new session from a given token. Most bots should be using
// NewWithIntents instead.
func New(token string) *Session {
	return NewWithIdentifier(gateway.DefaultIdentifier(token))
}

// Login tries to log in as a normal user account; MFA is optional.
func Login(ctx context.Context, email, password, mfa string) (*Session, error) {
	// Make a scratch HTTP client without a token
	client := api.NewClient("").WithContext(ctx)

	// Try to login without TOTP
	l, err := client.Login(email, password)
	if err != nil {
		return nil, errors.Wrap(err, "failed to login")
	}

	if l.Token != "" && !l.MFA {
		// We got the token, return with a new Session.
		return New(l.Token), nil
	}

	// Discord requests MFA, so we need the MFA token.
	if mfa == "" {
		return nil, ErrMFA
	}

	// Retry logging in with a 2FA token
	l, err = client.TOTP(mfa, l.Ticket)
	if err != nil {
		return nil, errors.Wrap(err, "failed to login with 2FA")
	}

	return New(l.Token), nil
}

// NewWithIdentifier creates a bare Session with the given identifier.
func NewWithIdentifier(id gateway.Identifier) *Session {
	return NewCustom(id, api.NewClient(id.Token), handler.New())
}

// NewWithGateway constructs a bare Session from the given UNOPENED gateway.
func NewWithGateway(g *gateway.Gateway, h *handler.Handler) *Session {
	state := g.State()
	return &Session{
		Client:  api.NewClient(state.Identifier.Token),
		Handler: h,
		state: &sessionState{
			gateway: g,
			id:      state.Identifier,
		},
	}
}

// NewCustom constructs a bare Session from the given parameters.
func NewCustom(id gateway.Identifier, cl *api.Client, h *handler.Handler) *Session {
	return &Session{
		Client:  cl,
		Handler: h,
		state:   &sessionState{id: id},
	}
}

// AddIntents adds the given intents into the gateway. Calling it after Open has
// already been called will result in a panic.
func (s *Session) AddIntents(intents gateway.Intents) {
	s.state.Lock()

	s.state.id.AddIntents(intents)

	if s.state.gateway != nil {
		s.state.gateway.AddIntents(intents)
	}

	s.state.Unlock()
}

// HasIntents reports if the Gateway has the passed Intents.
//
// If no intents are set, e.g. if using a user account, HasIntents will always
// return true.
func (s *Session) HasIntents(intents gateway.Intents) bool {
	return s.state.id.HasIntents(intents)
}

// Gateway returns the current session's gateway. If Open has never been called
// or Session was never constructed with a gateway, then nil is returned.
func (s *Session) Gateway() *gateway.Gateway {
	s.state.Lock()
	g := s.state.gateway
	s.state.Unlock()
	return g
}

// Open opens the Discord gateway and its handler, then waits until either the
// Ready or Resumed event gets through.
func (s *Session) Open(ctx context.Context) error {
	evCh := make(chan interface{})

	s.state.Lock()
	defer s.state.Unlock()

	if s.state.cancel != nil {
		if err := s.close(ctx); err != nil {
			return err
		}
	}

	if s.state.gateway == nil {
		g, err := gateway.NewWithIdentifier(ctx, s.state.id)
		if err != nil {
			return err
		}
		s.state.gateway = g
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.state.ctx = ctx
	s.state.cancel = cancel

	// TODO: change this to AddSyncHandler.
	rm := s.AddHandler(evCh)
	defer rm()

	opCh := s.state.gateway.Connect(s.state.ctx)
	s.state.doneCh = ophandler.Loop(opCh, s.Handler)

	for {
		select {
		case <-ctx.Done():
			s.close(ctx)
			return ctx.Err()

		case <-s.state.doneCh:
			// Event loop died.
			return s.state.gateway.LastError()

		case ev := <-evCh:
			switch ev.(type) {
			case *gateway.ReadyEvent, *gateway.ResumedEvent:
				return nil
			}
		}
	}
}

// WithContext returns a shallow copy of Session with the context replaced in
// the API client. All methods called on the returned Session will use this
// given context.
//
// This method is thread-safe only after Open and before Close are called. Open
// and Close should not be called on the returned Session.
func (s *Session) WithContext(ctx context.Context) *Session {
	cpy := *s
	cpy.Client = s.Client.WithContext(ctx)
	return &cpy
}

// Close closes the underlying Websocket connection, invalidating the session
// ID. It will send a closing frame before ending the connection, closing it
// gracefully. This will cause the bot to appear as offline instantly. To
// prevent this behavior, change Gateway.AlwaysCloseGracefully.
func (s *Session) Close() error {
	s.state.Lock()
	defer s.state.Unlock()

	return s.close(context.Background())
}

func (s *Session) close(ctx context.Context) error {
	if s.state.cancel == nil {
		return errors.New("Session is already closed")
	}

	s.state.cancel()
	s.state.cancel = nil
	s.state.ctx = nil

	// Wait until we've successfully disconnected.
	select {
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "cannot wait for gateway exit")
	case <-s.state.doneCh:
		// ok
	}

	s.state.doneCh = nil

	return s.state.gateway.LastError()
}
