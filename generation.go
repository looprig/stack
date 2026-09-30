package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/storage"
)

// generationKey is the control KV key holding a HostID's last generation.
// The HostID is hashed because a Core id admits bytes a storage name does
// not.
func generationKey(hostID sessionwire.HostID) string {
	sum := sha256.Sum256([]byte(hostID))
	return "stack/host-generation/" + hex.EncodeToString(sum[:])
}

// maxGenerationAttempts bounds the compare-and-swap loop. Two processes
// racing for one HostID is a misconfiguration; the bound turns it into an
// error rather than a spin.
const maxGenerationAttempts = 8

// nextGeneration advances the persisted generation counter for hostID and
// returns the new value. A Host generation must strictly increase across
// restarts of one HostID; a durable counter does that without trusting the
// wall clock.
func nextGeneration(ctx context.Context, kv storage.KV, hostID sessionwire.HostID) (uint64, error) {
	key := generationKey(hostID)
	for range maxGenerationAttempts {
		var current, revision uint64
		value, rev, err := kv.Get(ctx, key)
		var missing *storage.KeyNotFoundError
		switch {
		case errors.As(err, &missing):
		case err != nil:
			return 0, fmt.Errorf("stack: read host generation: %w", err)
		default:
			current, err = strconv.ParseUint(string(value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("stack: host generation at %s is corrupt: %w", key, err)
			}
			revision = rev
		}
		next := current + 1
		if next == 0 {
			return 0, fmt.Errorf("stack: host generation at %s is exhausted", key)
		}
		_, err = kv.Put(ctx, key, revision, []byte(strconv.FormatUint(next, 10)))
		var conflict *storage.ConflictError
		if errors.As(err, &conflict) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("stack: advance host generation: %w", err)
		}
		return next, nil
	}
	return 0, fmt.Errorf("stack: host generation for %q is contended; is another process running with the same HostID?", hostID)
}
