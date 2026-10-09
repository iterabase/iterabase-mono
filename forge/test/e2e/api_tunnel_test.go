package e2e

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// sshAPITunnel keeps the fixture's Kubernetes API private. Fixture hosts
// expose only pinned SSH; all client-go and kubectl traffic traverses one
// fixture-scoped direct-tcpip tunnel to the host-local K3s API.
type sshAPITunnel struct {
	address  string
	keyPath  string
	mu       sync.Mutex
	client   *ssh.Client
	listener net.Listener
	done     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
}

// sshTunnelKeepalive keeps the runner's outbound NAT mapping alive (GitHub
// runners drop idle flows after about four minutes without a reset) and detects
// a dead connection so the tunnel re-dials instead of stalling watches.
const sshTunnelKeepalive = 20 * time.Second

// fixturesByForgeHome lets runForgeE re-bind the kubeconfig Forge refreshes on
// every apply without each stage remembering to do so.
var fixturesByForgeHome sync.Map

func registerFixtureForgeHome(forgeHome string, fixture *hostFixture) {
	fixturesByForgeHome.Store(forgeHome, fixture)
}

// rebindForgeHomeKubeconfigs points every run kubeconfig under forgeHome at the
// fixture tunnel. A missing kubeconfig (apply refused before K3s) is not an error.
func rebindForgeHomeKubeconfigs(forgeHome string) error {
	value, ok := fixturesByForgeHome.Load(forgeHome)
	if !ok {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(forgeHome, "*", "kubeconfig.yaml"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := value.(*hostFixture).bindKubeconfig(path); err != nil {
			return fmt.Errorf("bind %s to the fixture API tunnel: %w", path, err)
		}
	}
	return nil
}

func (fixture *hostFixture) bindKubeconfig(path string) error {
	fixture.tunnelMu.Lock()
	defer fixture.tunnelMu.Unlock()
	if fixture.apiTunnel == nil {
		tunnel, err := startSSHAPITunnel(fixture.address, fixture.sshKeyPath)
		if err != nil {
			return fmt.Errorf("open pinned SSH tunnel to fixture Kubernetes API: %w", err)
		}
		fixture.apiTunnel = tunnel
	}
	serverName, err := rewriteKubeconfigForAPITunnel(path, fixture.apiTunnel.listener.Addr().String(), fixture.apiServerName)
	if err != nil {
		return err
	}
	fixture.apiServerName = serverName
	return nil
}

// dialHostLocal reaches a host-local port (for example an ingress NodePort)
// through the fixture's pinned SSH connection.
func (fixture *hostFixture) dialHostLocal(port int) (net.Conn, error) {
	fixture.tunnelMu.Lock()
	tunnel := fixture.apiTunnel
	fixture.tunnelMu.Unlock()
	if tunnel == nil {
		return nil, fmt.Errorf("fixture API tunnel is not open")
	}
	return tunnel.current().Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

func (fixture *hostFixture) stopAPITunnel() {
	fixture.tunnelMu.Lock()
	defer fixture.tunnelMu.Unlock()
	if fixture.apiTunnel == nil {
		return
	}
	fixture.apiTunnel.close()
	fixture.apiTunnel = nil
}

func (state *gpuFixtureState) bindKubeconfigTunnel(t *testing.T) {
	t.Helper()
	if err := state.fixture.bindKubeconfig(filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml")); err != nil {
		t.Fatalf("bind GPU fixture kubeconfig to pinned SSH tunnel: %v", err)
	}
}

func startSSHAPITunnel(address, keyPath string) (*sshAPITunnel, error) {
	client, err := dialHostLocalAPI(address, keyPath)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("listen for local Kubernetes API traffic: %w", err)
	}
	tunnel := &sshAPITunnel{address: address, keyPath: keyPath, client: client, listener: listener,
		done: make(chan struct{}), stop: make(chan struct{})}
	go tunnel.accept()
	go tunnel.keepalive()
	return tunnel, nil
}

// dialHostLocalAPI fails before a tunnel is used when the host-local K3s API is
// not reachable through the authenticated fixture connection.
func dialHostLocalAPI(address, keyPath string) (*ssh.Client, error) {
	client, err := sshDial(address, keyPath)
	if err != nil {
		return nil, err
	}
	probe, err := client.Dial("tcp", "127.0.0.1:6443")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("dial host-local K3s API: %w", err)
	}
	probe.Close()
	return client, nil
}

func (tunnel *sshAPITunnel) current() *ssh.Client {
	tunnel.mu.Lock()
	defer tunnel.mu.Unlock()
	return tunnel.client
}

// redial replaces a dead SSH connection. Closing the old client fails its
// forwarded connections promptly, so clients retry instead of hanging.
func (tunnel *sshAPITunnel) redial(dead *ssh.Client) {
	tunnel.mu.Lock()
	defer tunnel.mu.Unlock()
	if tunnel.client != dead {
		return // another caller already replaced it
	}
	_ = dead.Close()
	if client, err := dialHostLocalAPI(tunnel.address, tunnel.keyPath); err == nil {
		tunnel.client = client
	}
}

func (tunnel *sshAPITunnel) keepalive() {
	ticker := time.NewTicker(sshTunnelKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-tunnel.stop:
			return
		case <-ticker.C:
		}
		client := tunnel.current()
		replied := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			replied <- err
		}()
		select {
		case err := <-replied:
			if err != nil {
				tunnel.redial(client)
			}
		case <-time.After(sshTunnelKeepalive):
			tunnel.redial(client)
		case <-tunnel.stop:
			return
		}
	}
}

func (tunnel *sshAPITunnel) accept() {
	defer close(tunnel.done)
	for {
		local, err := tunnel.listener.Accept()
		if err != nil {
			return
		}
		client := tunnel.current()
		remote, err := client.Dial("tcp", "127.0.0.1:6443")
		if err != nil {
			tunnel.redial(client)
			remote, err = tunnel.current().Dial("tcp", "127.0.0.1:6443")
		}
		if err != nil {
			local.Close()
			continue
		}
		go proxyTunnelConnection(local, remote)
	}
}

func proxyTunnelConnection(local, remote net.Conn) {
	var closeOnce sync.Once
	closeBoth := func() {
		_ = local.Close()
		_ = remote.Close()
	}
	go func() {
		_, _ = io.Copy(local, remote)
		closeOnce.Do(closeBoth)
	}()
	_, _ = io.Copy(remote, local)
	closeOnce.Do(closeBoth)
}

func (tunnel *sshAPITunnel) close() {
	tunnel.stopOnce.Do(func() {
		close(tunnel.stop)
		_ = tunnel.listener.Close()
		_ = tunnel.current().Close()
		<-tunnel.done
	})
}

// rewriteKubeconfigForAPITunnel preserves the API certificate's original DNS/IP
// identity while replacing only its transport endpoint with the local tunnel.
// expectedServerName carries that identity across later Forge applies, each of
// which deliberately refreshes the kubeconfig from the host.
func rewriteKubeconfigForAPITunnel(path, localAddress, expectedServerName string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return "", err
	}
	contextName := cfg.CurrentContext
	context := cfg.Contexts[contextName]
	if context == nil {
		return "", fmt.Errorf("current kubeconfig context %q is missing", contextName)
	}
	cluster := cfg.Clusters[context.Cluster]
	if cluster == nil {
		return "", fmt.Errorf("kubeconfig cluster %q is missing", context.Cluster)
	}
	if cluster.Server == "https://"+localAddress && cluster.TLSServerName != "" {
		return cluster.TLSServerName, nil // already bound; Forge has not refreshed it since
	}
	serverName := expectedServerName
	if serverName == "" {
		server, parseErr := url.Parse(cluster.Server)
		if parseErr != nil || server.Hostname() == "" {
			return "", fmt.Errorf("parse original Kubernetes API server %q", cluster.Server)
		}
		serverName = server.Hostname()
	}
	cluster.Server = "https://" + localAddress
	cluster.TLSServerName = serverName
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		return "", err
	}
	return serverName, nil
}

func TestRewriteKubeconfigForAPITunnel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	cfg := clientcmdapi.Config{
		CurrentContext: "fixture",
		Contexts: map[string]*clientcmdapi.Context{
			"fixture": {Cluster: "fixture"},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			"fixture": {Server: "https://149.36.0.109:6443"},
		},
	}
	if err := clientcmd.WriteToFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	serverName, err := rewriteKubeconfigForAPITunnel(path, "127.0.0.1:32123", "")
	if err != nil {
		t.Fatal(err)
	}
	if serverName != "149.36.0.109" {
		t.Fatalf("server name = %q, want original fixture address", serverName)
	}
	got, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cluster := got.Clusters["fixture"]
	if cluster.Server != "https://127.0.0.1:32123" || cluster.TLSServerName != serverName {
		t.Fatalf("rewritten cluster = %#v", cluster)
	}
	// A second bind without an intervening Forge apply must keep the original
	// certificate identity rather than adopting the tunnel address.
	again, err := rewriteKubeconfigForAPITunnel(path, "127.0.0.1:32123", "")
	if err != nil || again != "149.36.0.109" {
		t.Fatalf("rebind server name = %q, %v; want original fixture address", again, err)
	}
}
