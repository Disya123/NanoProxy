package proxy

import "sync/atomic"

// KeyProvider holds the upstream NanoGPT API key in memory so it can be
// hot-swapped from the admin UI without restarting the process. Reads are
// lock-free; writes are atomic store. The proxy reads the current key on
// every upstream request, so a Set() takes effect immediately for all
// in-flight and subsequent calls.
//
// On boot the binary seeds this from either the env NANOGPT_API_KEY (one-shot
// bootstrap) or the value previously stored in the settings table.
type KeyProvider struct {
	v atomic.Pointer[string]
}

// NewKeyProvider returns a provider pre-loaded with the given key. An empty
// initial key is allowed — the proxy will return 502 on upstream calls until
// the operator saves one through the admin UI.
func NewKeyProvider(initial string) *KeyProvider {
	kp := &KeyProvider{}
	kp.Set(initial)
	return kp
}

// Get returns the current upstream API key. May be empty if not configured.
func (k *KeyProvider) Get() string {
	if p := k.v.Load(); p != nil {
		return *p
	}
	return ""
}

// Set atomically replaces the upstream API key. The next request through
// the proxy will pick up the new value; in-flight requests keep using
// whatever key was active when they started (their Authorization header is
// already serialized).
func (k *KeyProvider) Set(s string) {
	k.v.Store(&s)
}

// IsSet reports whether a non-empty key is currently loaded.
func (k *KeyProvider) IsSet() bool { return k.Get() != "" }
