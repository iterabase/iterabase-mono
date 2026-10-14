package sshprovisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// DeleteLoadBalancerServices deletes every Service of type LoadBalancer and
// waits for each deletion. On a hosted environment this is the first teardown
// step: the AWS Load Balancer Controller then removes the NLB, its target
// groups and its security groups, which Terraform does not own (HOR-593).
func (p *SSHProvisioner) DeleteLoadBalancerServices(ctx context.Context) ([]string, error) {
	out, err := p.run(ctx, kubectlCmd("get", "services", "--all-namespaces", "-o", "json"))
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Type string `json:"type"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse services: %w", err)
	}
	var deleted []string
	for _, svc := range list.Items {
		if svc.Spec.Type != "LoadBalancer" {
			continue
		}
		if _, err := p.run(ctx, kubectlCmd("delete", "service", svc.Metadata.Name, "-n", svc.Metadata.Namespace, "--ignore-not-found", "--timeout=5m")); err != nil {
			return deleted, fmt.Errorf("delete service %s/%s: %w", svc.Metadata.Namespace, svc.Metadata.Name, err)
		}
		deleted = append(deleted, svc.Metadata.Namespace+"/"+svc.Metadata.Name)
	}
	return deleted, nil
}

// ForwardLocal accepts connections on ln and forwards each to remote as seen
// from the host, through the verified SSH connection, until ctx ends or ln
// closes. Used by forge tunnel for the Kubernetes API of hosted environments.
func (p *SSHProvisioner) ForwardLocal(ctx context.Context, ln net.Listener, remote string) error {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer local.Close()
			upstream, err := client.Dial("tcp", remote)
			if err != nil {
				return
			}
			defer upstream.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, local); done <- struct{}{} }()
			go func() { _, _ = io.Copy(local, upstream); done <- struct{}{} }()
			<-done
		}()
	}
}
