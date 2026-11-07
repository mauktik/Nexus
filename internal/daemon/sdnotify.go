package daemon

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// notifier implements the sd_notify protocol without libsystemd: one
// datagram per state message to the AF_UNIX socket named by NOTIFY_SOCKET. A
// name starting with "@" is a Linux abstract socket; Go's net package maps
// the "@" to the leading NUL. With NOTIFY_SOCKET unset the protocol is a
// no-op. See sd_notify(3).
type notifier struct {
	addr string
}

func newNotifier(addr string) *notifier {
	return &notifier{addr: addr}
}

// notifyWriteTimeout bounds one send. A datagram write to a live socket does
// not block, so this only guards against a wedged receiver.
const notifyWriteTimeout = time.Second

// notify sends one state message, such as "READY=1".
func (n *notifier) notify(state string) error {
	if n.addr == "" {
		return nil
	}
	if !strings.HasPrefix(n.addr, "/") && !strings.HasPrefix(n.addr, "@") {
		// vsock: addresses are not used by a systemd service manager on the
		// same host; refuse rather than guess.
		return fmt.Errorf("notify socket %q: unsupported address", n.addr)
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: n.addr, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify socket: %w", err)
	}
	err = conn.SetWriteDeadline(time.Now().Add(notifyWriteTimeout))
	if err == nil {
		_, err = conn.Write([]byte(state))
	}
	if cerr := conn.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if err != nil {
		return fmt.Errorf("notify %q: %w", state, err)
	}
	return nil
}
