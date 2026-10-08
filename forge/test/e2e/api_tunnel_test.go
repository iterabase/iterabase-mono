package e2e

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// sshAPITunnel keeps the GPU fixture's Kubernetes API private. The
// fixture exposes only pinned SSH; all client-go and kubectl traffic traverses
// one fixture-scoped direct-tcpip tunnel to the host-local K3s API.
type sshAPITunnel struct {
	client   *ssh.Client
	listener net.Listener
	done     chan struct{}
	stopOnce sync.Once
}

func (state *gpuFixtureState) bindKubeconfigTunnel(t *testing.T) {
	t.Helper()
	if state.fixture == nil {
		return // legacy non-fixture qualification paths retain their existing public API path
	}
	if state.apiTunnel == nil {
		tunnel, err := startSSHAPITunnel(state.host.IP, state.privKeyPath)
		if err != nil {
			t.Fatalf("open pinned SSH tunnel to GPU fixture Kubernetes API: %v", err)
		}
		state.apiTunnel = tunnel
	}
	path := filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml")
	serverName, err := rewriteKubeconfigForAPITunnel(path, state.apiTunnel.listener.Addr().String(), state.apiServerName)
	if err != nil {
		t.Fatalf("bind GPU fixture kubeconfig to pinned SSH tunnel: %v", err)
	}
	state.apiServerName = serverName
}

func startSSHAPITunnel(address, keyPath string) (*sshAPITunnel, error) {
	client, err := sshDial(address, keyPath)
	if err != nil {
		return nil, err
	}
	// Fail before returning a local listener when the host-local K3s API is not
	// reachable through the authenticated fixture connection.
	probe, err := client.Dial("tcp", "127.0.0.1:6443")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("dial host-local K3s API: %w", err)
	}
	probe.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("listen for local Kubernetes API traffic: %w", err)
	}
	tunnel := &sshAPITunnel{client: client, listener: listener, done: make(chan struct{})}
	go tunnel.accept()
	return tunnel, nil
}

func (tunnel *sshAPITunnel) accept() {
	defer close(tunnel.done)
	for {
		local, err := tunnel.listener.Accept()
		if err != nil {
			return
		}
		remote, err := tunnel.client.Dial("tcp", "127.0.0.1:6443")
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

func (state *gpuFixtureState) stopAPITunnel() {
	if state.apiTunnel == nil {
		return
	}
	state.apiTunnel.stop()
	state.apiTunnel = nil
}

func (tunnel *sshAPITunnel) stop() {
	tunnel.stopOnce.Do(func() {
		_ = tunnel.listener.Close()
		_ = tunnel.client.Close()
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
}
