//go:build unix

package launch

import (
	"fmt"
	"os"

	"pix/host/hostenv"
	"pix/host/lease"
)

// retirePreviousCreation runs under RunSession's lifecycle lock, before sbx
// creates anything. A missing record is a fresh lifetime; an existing record
// can be retired only after a fresh JSON listing proves this name absent.
func retirePreviousCreation(env hostenv.Env, key, name string) error {
	dir, err := leaseDirFor(key)
	if err != nil {
		return err
	}
	rec, err := lease.ReadRecord(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if rec.Name != "" && rec.Name != name {
		return fmt.Errorf("previous creation record belongs to %q, not %q", rec.Name, name)
	}
	if err := lease.RetireAbsentInstance(dir, func() bool {
		entry, trusted := sbxEntry(env, name, TeardownProbeTimeout)
		return trusted && entry == nil
	}); err != nil {
		return fmt.Errorf("cannot prepare %q for a new session: %w", name, err)
	}
	return nil
}
