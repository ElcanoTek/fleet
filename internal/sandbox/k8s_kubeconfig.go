// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// k8s_kubeconfig.go loads client credentials for the kubernetes sandbox
// backend from a kubeconfig file — the out-of-cluster path used when the
// fleet control plane runs OUTSIDE the cluster that hosts its sandbox pods
// (a dev box pointed at kind, or a single-box install delegating runners to
// a cluster). In-cluster service-account auth (the production Helm path)
// lives in newInClusterClient (k8s_client.go).
//
// Deliberately minimal: current-context resolution, token / token-file /
// client-certificate credentials, CA bundles inline or by path. exec
// plugins and auth-providers are REFUSED with an actionable error rather
// than half-supported — the fleet process runs unattended, and shelling out
// to an interactive credential helper on every token expiry is exactly the
// kind of silent-degradation surface the fail-closed posture forbids.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

// kubeconfigFile mirrors the subset of the kubeconfig v1 schema the loader
// reads. Unknown fields are ignored by the YAML decoder, EXCEPT the exec /
// auth-provider blocks, which are decoded precisely so they can be refused.
type kubeconfigFile struct {
	CurrentContext string `yaml:"current-context"`
	Contexts       []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificate     string         `yaml:"client-certificate"`
			ClientCertificateData string         `yaml:"client-certificate-data"`
			ClientKey             string         `yaml:"client-key"`
			ClientKeyData         string         `yaml:"client-key-data"`
			Token                 string         `yaml:"token"`
			TokenFile             string         `yaml:"tokenFile"`
			Exec                  map[string]any `yaml:"exec"`
			AuthProvider          map[string]any `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
}

// newKubeconfigClient builds a k8sClient from the kubeconfig at path,
// following its current-context. The returned namespace is the context's
// default namespace ("" when the context sets none).
func newKubeconfigClient(path string) (*k8sClient, string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured kubeconfig path
	if err != nil {
		return nil, "", fmt.Errorf("read kubeconfig: %w", err)
	}
	var kc kubeconfigFile
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, "", fmt.Errorf("parse kubeconfig %s: %w", path, err)
	}
	if kc.CurrentContext == "" {
		return nil, "", fmt.Errorf("kubeconfig %s has no current-context", path)
	}
	var clusterName, userName, namespace string
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			clusterName, userName, namespace = c.Context.Cluster, c.Context.User, c.Context.Namespace
			break
		}
	}
	if clusterName == "" {
		return nil, "", fmt.Errorf("kubeconfig %s: current-context %q not found", path, kc.CurrentContext)
	}

	// Relative CA / cert / key paths in a kubeconfig are relative to the FILE,
	// not the process cwd — kubectl's convention, kept so the same file works.
	baseDir := filepath.Dir(path)
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	server, err := applyKubeconfigCluster(&kc, clusterName, path, resolve, tlsCfg)
	if err != nil {
		return nil, "", err
	}

	client := &k8sClient{tlsConfig: tlsCfg}
	if err := applyKubeconfigUser(&kc, userName, path, resolve, tlsCfg, client); err != nil {
		return nil, "", err
	}

	base, err := url.Parse(server)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig %s: parse server URL %q: %w", path, server, err)
	}
	if base.Scheme != "https" {
		return nil, "", fmt.Errorf("kubeconfig %s: server %q is not https — the sandbox control channel requires TLS (fail-closed)", path, server)
	}
	client.baseURL = base
	client.httpc = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	return client, namespace, nil
}

// applyKubeconfigCluster resolves the named cluster's server URL and installs
// its CA bundle into tlsCfg. insecure-skip-tls-verify is refused rather than
// honored: a sandbox control channel that skips server verification can be
// MITM'd into running tool calls on an attacker's cluster.
func applyKubeconfigCluster(kc *kubeconfigFile, clusterName, path string, resolve func(string) string, tlsCfg *tls.Config) (server string, err error) {
	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		server = c.Cluster.Server
		if c.Cluster.InsecureSkipTLSVerify {
			return "", fmt.Errorf("kubeconfig %s: cluster %q sets insecure-skip-tls-verify, which the sandbox backend refuses (fail-closed) — use a CA bundle", path, clusterName)
		}
		caPEM := []byte(nil)
		switch {
		case c.Cluster.CertificateAuthorityData != "":
			caPEM, err = base64.StdEncoding.DecodeString(c.Cluster.CertificateAuthorityData)
			if err != nil {
				return "", fmt.Errorf("kubeconfig %s: decode certificate-authority-data: %w", path, err)
			}
		case c.Cluster.CertificateAuthority != "":
			caPEM, err = os.ReadFile(resolve(c.Cluster.CertificateAuthority))
			if err != nil {
				return "", fmt.Errorf("kubeconfig %s: read certificate-authority: %w", path, err)
			}
		}
		if caPEM != nil {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				return "", fmt.Errorf("kubeconfig %s: cluster %q CA bundle contains no usable certificates", path, clusterName)
			}
			tlsCfg.RootCAs = pool
		}
		break
	}
	if server == "" {
		return "", fmt.Errorf("kubeconfig %s: cluster %q not found or has no server", path, clusterName)
	}
	return server, nil
}

// applyKubeconfigUser resolves the named user's credentials into the client
// (token / token-file) or tlsCfg (client certificate). exec plugins and
// auth-providers are refused — the fleet process runs unattended.
func applyKubeconfigUser(kc *kubeconfigFile, userName, path string, resolve func(string) string, tlsCfg *tls.Config, client *k8sClient) error {
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		if u.User.Exec != nil || u.User.AuthProvider != nil {
			return fmt.Errorf("kubeconfig %s: user %q uses an exec plugin / auth-provider, which the sandbox backend does not support — "+
				"create a ServiceAccount token or client certificate for fleet instead", path, userName)
		}
		certPEM, err := kubeconfigPEM(u.User.ClientCertificateData, resolve(u.User.ClientCertificate))
		if err != nil {
			return fmt.Errorf("kubeconfig %s: client-certificate: %w", path, err)
		}
		keyPEM, err := kubeconfigPEM(u.User.ClientKeyData, resolve(u.User.ClientKey))
		if err != nil {
			return fmt.Errorf("kubeconfig %s: client-key: %w", path, err)
		}
		switch {
		case certPEM != nil && keyPEM != nil:
			cert, cerr := tls.X509KeyPair(certPEM, keyPEM)
			if cerr != nil {
				return fmt.Errorf("kubeconfig %s: load client certificate: %w", path, cerr)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		case u.User.Token != "":
			client.staticToken = u.User.Token
		case u.User.TokenFile != "":
			client.tokenFile = resolve(u.User.TokenFile)
		default:
			return fmt.Errorf("kubeconfig %s: user %q has no supported credentials (token, tokenFile, or client certificate)", path, userName)
		}
		return nil
	}
	return fmt.Errorf("kubeconfig %s: user %q not found", path, userName)
}

// kubeconfigPEM loads a PEM blob from inline base64 data (preferred) or a
// file path; both empty returns (nil, nil).
func kubeconfigPEM(inlineB64, filePath string) ([]byte, error) {
	if inlineB64 != "" {
		data, err := base64.StdEncoding.DecodeString(inlineB64)
		if err != nil {
			return nil, fmt.Errorf("decode inline data: %w", err)
		}
		return data, nil
	}
	if filePath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filePath) //nolint:gosec // path from an operator-configured kubeconfig, not request input
	if err != nil {
		return nil, err
	}
	return data, nil
}
