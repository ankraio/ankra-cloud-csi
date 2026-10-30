package cloud

import (
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// silenceableListener hands out connections that can all be silenced at once: a silenced connection swallows what is
// written to it and never delivers another byte, like one whose packets a NAT started dropping.
type silenceableListener struct {
	net.Listener
	mutex    sync.Mutex
	accepted []*silenceableConn
}

func (listener *silenceableListener) Accept() (net.Conn, error) {
	conn, acceptError := listener.Listener.Accept()
	if acceptError != nil {
		return nil, acceptError
	}
	wrapped := &silenceableConn{Conn: conn, silenced: make(chan struct{}), closed: make(chan struct{})}
	listener.mutex.Lock()
	listener.accepted = append(listener.accepted, wrapped)
	listener.mutex.Unlock()
	return wrapped, nil
}

func (listener *silenceableListener) silenceAll() {
	listener.mutex.Lock()
	defer listener.mutex.Unlock()
	for _, conn := range listener.accepted {
		conn.silence()
	}
}

func (listener *silenceableListener) acceptedCount() int {
	listener.mutex.Lock()
	defer listener.mutex.Unlock()
	return len(listener.accepted)
}

type silenceableConn struct {
	net.Conn
	silenceOnce sync.Once
	closeOnce   sync.Once
	silenced    chan struct{}
	closed      chan struct{}
}

func (conn *silenceableConn) silence() { conn.silenceOnce.Do(func() { close(conn.silenced) }) }

func (conn *silenceableConn) isSilenced() bool {
	select {
	case <-conn.silenced:
		return true
	default:
		return false
	}
}

func (conn *silenceableConn) Read(buffer []byte) (int, error) {
	if conn.isSilenced() {
		<-conn.closed
		return 0, net.ErrClosed
	}
	return conn.Conn.Read(buffer)
}

func (conn *silenceableConn) Write(buffer []byte) (int, error) {
	if conn.isSilenced() {
		return len(buffer), nil
	}
	return conn.Conn.Write(buffer)
}

func (conn *silenceableConn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closed) })
	return conn.Conn.Close()
}

// TestADeadConnectionIsReplaced is ankra-t9c7w.36.24: the CSI's one HTTP/2 connection to the API went silent and every
// later call was sent onto it and waited out the client timeout, for minutes, until the kernel reset the connection.
func TestADeadConnectionIsReplaced(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"storage":`+storageJSON+`}`)
	}))
	listener := &silenceableListener{Listener: server.Listener}
	server.Listener = listener
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	bundlePath := filepath.Join(t.TempDir(), "ca.pem")
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if writeError := os.WriteFile(bundlePath, bundle, 0o600); writeError != nil {
		t.Fatalf("write the CA bundle: %v", writeError)
	}
	const timeout = 5 * time.Second
	client, clientError := NewClient(Configuration{Endpoint: server.URL, Token: "act_test", CABundlePath: bundlePath,
		Timeout: timeout, PingInterval: 200 * time.Millisecond, PingTimeout: 200 * time.Millisecond})
	if clientError != nil {
		t.Fatalf("NewClient: %v", clientError)
	}
	ctx := context.Background()
	if _, getError := client.GetVolume(ctx, "vol-1"); getError != nil {
		t.Fatalf("first call: %v", getError)
	}

	listener.silenceAll()
	started := time.Now()
	// The call already on the silent connection fails once the ping goes unanswered; it does not wait out the timeout.
	_, _ = client.GetVolume(ctx, "vol-1")
	if waited := time.Since(started); waited >= timeout {
		t.Fatalf("the call on the silent connection waited %s, the whole client timeout", waited)
	}
	if _, getError := client.GetVolume(ctx, "vol-1"); getError != nil {
		t.Fatalf("the call after the silent connection: %v", getError)
	}
	if count := listener.acceptedCount(); count != 2 {
		t.Fatalf("the client opened %d connections, want 2: the silent one and its replacement", count)
	}
}
