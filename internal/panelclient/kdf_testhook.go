package panelclient

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/argon2"
)

// kdfMemo, when set, remembers derived keys. Only tests set it (MemoizeKDFForTests): the same password,
// salt and cost always give the same key, so a test that unlocks many times pays for Argon2id once.
var kdfMemo atomic.Pointer[kdfCache]

type kdfCache struct {
	mu   sync.Mutex
	keys map[string][]byte
}

// MemoizeKDFForTests makes the password KDF remember its results for the rest of the process. Call it from
// a test's TestMain. It does nothing outside a test binary, so the product can't use it.
func MemoizeKDFForTests() {
	if testing.Testing() {
		kdfMemo.CompareAndSwap(nil, &kdfCache{keys: map[string][]byte{}})
	}
}

func (c *kdfCache) derive(password string, salt []byte, k PasswordKDF) ([]byte, error) {
	id := fmt.Sprintf("%d|%d|%d|%x|%s", k.Time, k.MemoryKiB, k.Threads, salt, password)
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[id]; ok {
		return append([]byte(nil), key...), nil
	}
	key := argon2.IDKey([]byte(password), salt, k.Time, k.MemoryKiB, k.Threads, 32)
	c.keys[id] = key
	return append([]byte(nil), key...), nil
}
