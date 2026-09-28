package daemon

import (
	"fmt"
	"log/slog"
	"net"
	"os"
)

// uidListener drops connections whose peer UID differs from ours.
type uidListener struct {
	net.Listener
	uid int
	log *slog.Logger
}

func (l *uidListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := checkPeer(c, l.uid); err != nil {
			l.log.Warn("rejected socket peer", "err", err)
			c.Close()
			continue
		}
		return c, nil
	}
}

func checkPeer(c net.Conn, want int) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var uid int
	var perr error
	if err := raw.Control(func(fd uintptr) { uid, perr = peerUID(fd) }); err != nil {
		return err
	}
	if perr != nil {
		return perr
	}
	if uid != want {
		return fmt.Errorf("peer uid %d does not match daemon uid %d", uid, want)
	}
	return nil
}

func currentUID() int { return os.Getuid() }
