package routing

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strings"
	"sync"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Content-addressed cache for image descriptions.
//
// An agentic client re-sends its own transcript on every turn, image parts
// included. Without a cache the rescue in captionImages therefore describes
// the same image again on every turn of a conversation that carries one — the
// image is byte-identical, the description is not. This cache makes the second
// and every later turn with the SAME question free, and keeps the substituted
// text identical while that remains true, which is also what keeps an upstream
// prompt cache valid: a history position whose text changes between turns
// invalidates the prefix from there on. A changed question is a different
// description (see below), and the prefix invalidation that follows is the
// price of answering the question actually asked.
//
// The key is content-addressed (the image's decoded bytes plus the task hint
// the description was asked for), so the same image reaching the proxy as a
// data URI, as a URL that got inlined first, or in a later turn always maps to
// the same entry — as long as the question is the same. A description is a
// property of the image AND of the question that shaped it: a later turn
// asking something new must be described for that new question, not handed the
// earlier question's text.
//
// One thing is deliberately NOT part of the key:
//
//   - the model that happened to answer. The describer list is consulted in
//     order and its head changes with health, so keying on the responder would
//     fragment the cache by outage. The configured list is what the key
//     carries: an operator editing the list invalidates, a failover inside it
//     does not.
//
// Failures are never stored. A description that could not be produced must not
// harden into a permanent answer.

// captionCacheEntries bounds the cache. An entry is one description plus its
// key, so the ceiling is a few hundred kilobytes of text — small enough to
// leave unbounded in time, which is why there is no TTL: the key names content,
// which cannot go stale.
const captionCacheEntries = 256

// captionKey identifies one cached description.
type captionKey struct {
	// clientKeyID keeps descriptions from crossing between clients. The
	// describer is shown the client's question as a steering hint, and model
	// output can quote what it is shown, so a shared bucket would let one
	// client's wording ride into another client's request. Empty when auth is
	// disabled, which is a single-tenant mode by definition.
	clientKeyID string

	// version covers everything in the describer configuration that shapes the
	// text: the base prompt and the configured describer list.
	version string

	// task is the digest of the task hint appended to the prompt (the client's
	// text that the describer was asked to focus on), truncated exactly as it
	// is sent. Zero when the task-aware switch is off. Without it, a later turn
	// with a different question would hit the entry written for the earlier
	// one and the answering model would be told what the image showed in terms
	// of a question nobody is asking any more.
	task [32]byte

	// digest is the SHA-256 of the image's decoded bytes.
	digest [32]byte
}

// captionEntry is one cached description.
type captionEntry struct {
	// key rides the entry so eviction can delete the map slot without
	// scanning the map for the element it just unlinked.
	key captionKey

	text string

	// providerID and model name the describer that actually produced the text.
	// A cache hit reports these rather than the currently configured head of
	// the list: the operator is owed the provenance of what the answering model
	// was told, not the identity of a model that was never called.
	providerID string
	model      string
}

// captionCache is a bounded least-recently-used map. The workload is one hot
// image per active conversation, so recency is the signal worth keeping.
//
// The mutex guards map and list only — never a network call, never the image
// fetch, and never the describer dispatch.
type captionCache struct {
	mu      sync.Mutex
	max     int
	entries map[captionKey]*list.Element // element.Value is *captionEntry
	order   *list.List                   // front is the most recently used
}

func newCaptionCache(max int) *captionCache {
	if max <= 0 {
		max = captionCacheEntries
	}
	return &captionCache{
		max:     max,
		entries: make(map[captionKey]*list.Element, max),
		order:   list.New(),
	}
}

// get returns the cached description for key, marking it as most recently used.
func (c *captionCache) get(key captionKey) (*captionEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	entry, _ := el.Value.(*captionEntry)
	return entry, entry != nil
}

// put stores a description, evicting the least recently used entry when the
// cache is full. An empty description is not stored: there is nothing to reuse
// and a stored blank would render as an answer.
func (c *captionCache) put(key captionKey, entry *captionEntry) {
	if c == nil || entry == nil || strings.TrimSpace(entry.text) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value = entry
		c.order.MoveToFront(el)
		return
	}
	c.entries[key] = c.order.PushFront(entry)
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		if k, ok := c.keyOf(oldest); ok {
			delete(c.entries, k)
		}
	}
}

// keyOf recovers the map key of a list element. The key rides the element so
// eviction does not have to scan the map for it.
func (c *captionCache) keyOf(el *list.Element) (captionKey, bool) {
	k, ok := el.Value.(*captionEntry)
	if !ok || k == nil {
		return captionKey{}, false
	}
	return k.key, true
}

// captionVersion renders the describer configuration that shapes a description.
// Only what the describer is told before the image enters: the base prompt and
// the model list. The task-aware suffix is not part of the version because it
// changes per request; it enters the key through captionImageKey's task digest.
func captionVersion(basePrompt string, models []ChainEntry) string {
	var b strings.Builder
	b.WriteString(basePrompt)
	for _, m := range models {
		b.WriteByte(0)
		b.WriteString(m.ProviderID)
		b.WriteByte('/')
		b.WriteString(m.Model)
	}
	return b.String()
}

// captionDigest hashes an image's decoded bytes. The wire carries base64; the
// decoded form is what makes a data URI and a fetched URL of the same image
// agree.
//
// A payload that is not canonical base64 (a client can send anything inside a
// data URI, and the codecs keep it verbatim) hashes over its text instead. That
// is still deterministic and still content-addressed; it only fails to unify
// two different encodings of the same picture, which is a missed reuse and
// never a wrong one.
func captionDigest(data string) [32]byte {
	h := sha256.New()
	dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
	if _, err := io.Copy(h, dec); err != nil {
		return sha256.Sum256([]byte(data))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// captionImageKey builds the cache key for one image. task is the task hint
// the describer was given (empty when the task-aware switch is off); it is
// hashed so the map does not retain the client's text.
func captionImageKey(clientKeyID, version, task string, img ir.ImagePart) captionKey {
	key := captionKey{
		clientKeyID: clientKeyID,
		version:     version,
		digest:      captionDigest(img.Data),
	}
	if task != "" {
		key.task = sha256.Sum256([]byte(task))
	}
	return key
}

// clientKeyCtxKey carries the client key identity into the executor. The
// identity is known where the request body is decoded, not where the chain is
// walked, so it rides the context rather than every signature between them.
type clientKeyCtxKey struct{}

// WithClientKeyID annotates ctx with the client key that authenticated the
// request. The cache uses it to keep one client's descriptions out of another
// client's requests; an unauthenticated deployment simply leaves it unset.
func WithClientKeyID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, clientKeyCtxKey{}, id)
}

// clientKeyIDFromContext reads what WithClientKeyID stored.
func clientKeyIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clientKeyCtxKey{}).(string)
	return id
}
