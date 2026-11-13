package integrations

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type Scanner interface {
	Scan(context.Context, []byte) (bool, error)
}
type ClamScanner struct {
	Address string
	TLS     *tls.Config
}

func NewScanner() (Scanner, error) {
	address := os.Getenv("CLAMAV_ADDRESS")
	if address == "" {
		return nil, errors.New("ClamAV address required")
	}
	var secure *tls.Config
	if path := os.Getenv("CLAMAV_CA_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, errors.New("invalid scanner CA")
		}
		name := os.Getenv("CLAMAV_SERVER_NAME")
		if name == "" {
			return nil, errors.New("scanner TLS server name required")
		}
		secure = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: name}
		if cert := os.Getenv("CLAMAV_CLIENT_CERT_FILE"); cert != "" {
			pair, err := tls.LoadX509KeyPair(cert, os.Getenv("CLAMAV_CLIENT_KEY_FILE"))
			if err != nil {
				return nil, err
			}
			secure.Certificates = []tls.Certificate{pair}
		}
	} else if os.Getenv("APP_ENV") != "development" {
		return nil, errors.New("scanner TLS required")
	}
	return &ClamScanner{address, secure}, nil
}
func (s *ClamScanner) Scan(ctx context.Context, data []byte) (bool, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var err error
	if s.TLS != nil {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: s.TLS}).DialContext(ctx, "tcp", s.Address)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.Address)
	}
	if err != nil {
		return false, errors.New("scanner unavailable")
	}
	defer conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	conn.SetDeadline(deadline)
	if _, err = io.WriteString(conn, "zINSTREAM\x00"); err != nil {
		return false, errors.New("scanner unavailable")
	}
	for offset := 0; offset < len(data); offset += 65536 {
		end := offset + 65536
		if end > len(data) {
			end = len(data)
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(end-offset))
		if _, err = conn.Write(size[:]); err != nil {
			return false, errors.New("scanner unavailable")
		}
		if _, err = conn.Write(data[offset:end]); err != nil {
			return false, errors.New("scanner unavailable")
		}
	}
	if _, err = conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return false, errors.New("scanner unavailable")
	}
	result, err := bufio.NewReader(io.LimitReader(conn, 2048)).ReadString(0)
	if err != nil {
		return false, errors.New("scanner incomplete response")
	}
	if result == "stream: OK\x00" {
		return true, nil
	}
	if strings.HasSuffix(result, " FOUND\x00") {
		return false, nil
	}
	return false, errors.New("scanner rejected stream")
}
