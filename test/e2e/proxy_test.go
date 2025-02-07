package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/certificates"
	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	// userCABundleName is the name of the ConfigMap that holds additional user-provided proxy certs
	userCABundleName      = "user-ca-bundle"
	userCABundleNamespace = "openshift-config"
)

// proxyTestSuite contains the validation cases for cluster-wide proxy.
// All subtests are skipped if a proxy is not enabled in the test environment.
func proxyTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	proxyEnabled, err := tc.client.ProxyEnabled()
	require.NoErrorf(t, err, "error checking if proxy is enabled in test environment")
	if !proxyEnabled {
		t.Skip("cluster-wide proxy is not enabled in this environment")
	}

	// Enables proxy test suite to be run individually on existing Windows nodes
	require.NoError(t, tc.loadExistingNodes())

	t.Run("Trusted CA ConfigMap validation", tc.testTrustedCAConfigMap)
	// Certificate validation test must run before environment variables removal validation since the latter results in
	// the deletion of TrustedCAConfigMap, which the former relies on
	t.Run("Certificate validation", tc.testCerts)
	t.Run("Environment variables validation", tc.testEnvVars)
}

// testCerts tests that any additional certificates from the proxy's trusted bundle are imported by each node
func (tc *testContext) testCerts(t *testing.T) {
	// TODO: this only tests the user-provided certs, a subset of the required proxy certificates.
	// Should be addressed with https://issues.redhat.com/browse/WINC-1144
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(userCABundleNamespace).Get(context.TODO(), userCABundleName, meta.GetOptions{})
	require.NoErrorf(t, err, "error getting user-provided CA ConfigMap: %w", err)

	// Read all expected certs from CM data
	trustedCABundle := cm.Data[certificates.CABundleKey]
	assert.Greater(t, len(trustedCABundle), 0, "no additional user-provided certs in bundle")

	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			// Read in one cert at a time and test it exists in the Windows instance's system store
			i := 0
			for block, rest := pem.Decode([]byte(trustedCABundle)); block != nil; block, rest = pem.Decode(rest) {
				certBytes := pem.EncodeToMemory(block)
				// Multi-line certificate data causes issues in the command. Encode to base64 as a workaround
				expectedCertBase64 := base64.StdEncoding.EncodeToString(certBytes)
				commandToRun := fmt.Sprintf("$base64Data=\\\"%s\\\";"+
					// Decode base64 into cert's actual string data
					"$certString=[Text.Encoding]::Utf8.GetString([Convert]::FromBase64String($base64Data));"+
					// Create a Powershell certificate object with the expected cert.
					// First requires data to be written to a file and then provide the file path the cert constructor
					"Set-Content C:\\Temp\\cert.pem $certString;"+
					"$expectedCert=[System.Security.Cryptography.X509Certificates.X509Certificate2]::new(\\\"C:\\Temp\\cert.pem\\\");"+
					// Get the number of existing certs equivalent to the expected cert
					"(Get-ChildItem -Path Cert:\\LocalMachine\\Root | Where-Object {$expectedCert.Equals($_)}).Count",
					expectedCertBase64)
				out, err := tc.runPowerShellSSHJob(fmt.Sprintf("get-cert-%d", i), commandToRun, addr)
				if err != nil {
					require.NoError(t, err, "error running SSH job: %w", err)
				}
				// Final line should contain a single number representing the number of certs found equal to the target
				count, err := strconv.Atoi(finalLine(out))
				require.NoError(t, err)

				assert.Equalf(t, count, 1, "unexpected cert %d count on node %s: expected 1, found %d", i, node, count)
				i++
			}
		})
	}
}

// testEnvVars tests that on each node
// 1. the system-level environment variables are set properly as per the cluster-wide proxy
// 2. the required Windows services pick up the proper values for proxy environment variables
func (tc *testContext) testEnvVars(t *testing.T) {
	clusterProxy, err := tc.client.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		require.NoError(t, err)
	}
	expectedEnvVars := map[string]string{}
	expectedEnvVars["HTTP_PROXY"] = clusterProxy.Status.HTTPProxy
	expectedEnvVars["HTTPS_PROXY"] = clusterProxy.Status.HTTPSProxy
	expectedEnvVars["NO_PROXY"] = clusterProxy.Status.NoProxy
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")
	watchedEnvVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			for _, proxyVar := range watchedEnvVars {
				t.Run(proxyVar, func(t *testing.T) {
					systemEnvVars, err := tc.getSystemEnvVar(addr, proxyVar)
					require.NoError(t, err, "unable to get value of %s from instance", proxyVar)
					assert.Equalf(t, expectedEnvVars[proxyVar], systemEnvVars[proxyVar], "incorrect value for %s", proxyVar)
				})
			}

			for _, svcName := range windows.RequiredServices {
				t.Run(svcName, func(t *testing.T) {
					svcEnvVars, err := tc.getProxyEnvVarsFromService(addr, svcName)
					require.NoErrorf(t, err, "error getting environment variables of service %s", svcName)
					for _, proxyVar := range watchedEnvVars {
						assert.Equalf(t, expectedEnvVars[proxyVar], svcEnvVars[proxyVar], "incorrect value for %s", proxyVar)
					}
				})
			}
		})
	}
	t.Run("Environment variables removal validation", tc.testEnvVarRemoval)
}

// testEnvVarRemoval tests that on each node the system-level and the process-level environment variables
// are unset when the cluster-wide proxy is disabled by patching the proxy variables in the cluster proxy object.
func (tc *testContext) testEnvVarRemoval(t *testing.T) {
	// This test is flaking consistently after we fix the way we check for proxy enabled in the e2e tests.
	// Until we address https://issues.redhat.com/browse/OCPBUGS-19502, this test has been temporarily disabled.
	t.Skip("Env var removal validation test is disabled, pending a fix")
	var patches []*patch.JSONPatch
	patches = append(patches, patch.NewJSONPatch("remove", "/spec/httpProxy", "httpProxy"),
		patch.NewJSONPatch("remove", "/spec/httpsProxy", "httpsProxy"))
	patchData, err := json.Marshal(patches)
	require.NoErrorf(t, err, "%v", patches)
	_, err = tc.client.Config.ConfigV1().Proxies().Patch(
		context.TODO(),
		"cluster",
		types.JSONPatchType,
		patchData,
		meta.PatchOptions{},
	)
	patchString := string(patchData)
	require.NoErrorf(t, err, "unable to patch %s", patchString)
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfMachineNodes, false, false)
	assert.NoError(t, err, "timed out waiting for Windows Machine nodes")
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, false, true)
	assert.NoError(t, err, "timed out waiting for BYOH Windows nodes")
	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			err := wait.PollImmediate(retry.Interval, retry.ResourceChangeTimeout, func() (done bool, err error) {
				foundNode, err := tc.client.K8s.CoreV1().Nodes().Get(context.TODO(), node.GetName(), meta.GetOptions{})
				require.NoError(t, err)
				return tc.nodeReadyAndSchedulable(*foundNode), nil
			})
			assert.NoError(t, err)
		})
	}
	for _, node := range gc.allNodes() {
		addr, err := controllers.GetAddress(node.Status.Addresses)
		require.NoError(t, err, "unable to get node address")
		envVarsRemoved, err := tc.checkEnvVarsRemoved(addr)
		require.NoError(t, err, "error determining if ENV vars are removed")
		assert.True(t, envVarsRemoved, "ENV vars not removed")
	}
}

// testTrustedCAConfigMap tests multiple aspects of expected functionality for the trusted-ca ConfigMap
// 1. It exists on operator startup 2. It is re-created when deleted 3. It is patched if invalid contents are detected.
// The ConfigMap data is managed by CNO so no need to do content validation testing.
func (tc *testContext) testTrustedCAConfigMap(t *testing.T) {
	// Ensure the trusted-ca ConfigMap exists in the cluster as expected
	t.Run("Trusted CA ConfigMap metadata", func(t *testing.T) {
		trustedCA, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
			certificates.ProxyCertsConfigMap, meta.GetOptions{})
		require.NoErrorf(t, err, "error ensuring ConfigMap %s exists", certificates.ProxyCertsConfigMap)
		assert.True(t, trustedCA.GetLabels()[controllers.InjectionRequestLabel] == "true")
	})

	t.Run("Trusted CA ConfigMap re-creation", func(t *testing.T) {
		err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Delete(context.TODO(),
			certificates.ProxyCertsConfigMap, meta.DeleteOptions{})
		require.NoError(t, err)
		err = tc.waitForValidTrustedCAConfigMap()
		assert.NoErrorf(t, err, "error ensuring ConfigMap %s is re-created when deleted", certificates.ProxyCertsConfigMap)
	})

	t.Run("Invalid trusted CA ConfigMap patching", func(t *testing.T) {
		// Intentionally remove the required label and wait for WMCO to reconcile and re-apply it
		var labelPatch = []*patch.JSONPatch{
			patch.NewJSONPatch("remove", "/metadata/labels", map[string]string{controllers.InjectionRequestLabel: "true"}),
		}
		patchData, err := json.Marshal(labelPatch)
		require.NoError(t, err)

		_, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Patch(context.TODO(), certificates.ProxyCertsConfigMap,
			types.JSONPatchType, patchData, meta.PatchOptions{})
		require.NoErrorf(t, err, "unable to patch %s", certificates.ProxyCertsConfigMap)
		err = tc.waitForValidTrustedCAConfigMap()
		assert.NoError(t, err, "error testing handling of invalid ConfigMap")
	})
}

// waitForValidTrustedCAConfigMap returns a reference to the ConfigMap that matches the given name.
// If a ConfigMap with valid contents is not found within the time limit, an error is returned.
func (tc *testContext) waitForValidTrustedCAConfigMap() error {
	trustedCA := &core.ConfigMap{}
	err := wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		var err error
		trustedCA, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
			certificates.ProxyCertsConfigMap, meta.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Retry if the Get() results in a IsNotFound error
				return false, nil
			}
			return false, fmt.Errorf("error retrieving ConfigMap %s: %w", certificates.ProxyCertsConfigMap, err)
		}
		// Here, we've retreived a ConfigMap but still need to ensure it is valid.
		// If it's not valid, retry in hopes that WMCO will replace it with a valid one as expected.
		return trustedCA.GetLabels()[controllers.InjectionRequestLabel] == "true", nil
	})
	if err != nil {
		return fmt.Errorf("error waiting for ConfigMap %s/%s: %w", wmcoNamespace, certificates.ProxyCertsConfigMap, err)
	}
	return nil
}

// getSystemEnvVar returns the value corresponding to the input proxy ENV var as set in the registry
func (tc *testContext) getSystemEnvVar(addr, variableName string) (map[string]string, error) {
	command := fmt.Sprintf("Get-ChildItem -Path Env: | Where-Object -Property Name -eq '%s' | Format-List ",
		variableName)
	return tc.getEnvVar(addr, variableName, command)
}

// getServiceProxyEnvVars returns a map of all environment variables present in a service's config
func (tc *testContext) getProxyEnvVarsFromService(addr, svcName string) (map[string]string, error) {
	command := fmt.Sprintf("Get-Process %s | ForEach-Object { $_.StartInfo.EnvironmentVariables.GetEnumerator() "+
		"| Format-List }",
		svcName)
	return tc.getEnvVar(addr, svcName, command)
}

func (tc *testContext) getEnvVar(addr, name, command string) (map[string]string, error) {
	out, err := tc.runPowerShellSSHJob(fmt.Sprintf("get-%s-env-vars",
		strings.ToLower(strings.ReplaceAll(name, "_", "-"))), command, addr)
	if err != nil {
		return nil, fmt.Errorf("error running SSH job: %w", err)
	}
	return parseWindowsEnvVars(out), nil
}

// configureUserCABundle configures the cluster-wide proxy with additional user-provided certificates
func (tc *testContext) configureUserCABundle() error {
	if err := tc.createUserCABundle(); err != nil {
		return err
	}
	return tc.patchProxyTrustedCA()
}

// createUserCABundle creates a ConfigMap with an additional trusted CA bundle
func (tc *testContext) createUserCABundle() error {
	cert, err := generateCertificate()
	if err != nil {
		return fmt.Errorf("unable to generate additional certs: %w", err)
	}
	userCABundleCM := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      userCABundleName,
			Namespace: userCABundleNamespace,
		},
		Data: map[string]string{
			certificates.CABundleKey: cert,
		},
	}
	_, err = tc.client.K8s.CoreV1().ConfigMaps(userCABundleNamespace).Create(context.TODO(), userCABundleCM, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating user-provided CA ConfigMap: %w", err)
	}
	return nil
}

// patchProxyTrustedCA patches the proxy to use the user-provided CA bundle, which CNO will merge with other proxy certs
func (tc *testContext) patchProxyTrustedCA() error {
	patches := []*patch.JSONPatch{patch.NewJSONPatch("replace", "/spec/trustedCA/name", userCABundleName)}
	patchData, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("invalid patch data %v: %w", patches, err)
	}
	_, err = tc.client.Config.ConfigV1().Proxies().Patch(context.TODO(), "cluster", types.JSONPatchType, patchData,
		meta.PatchOptions{})
	if err != nil {
		return fmt.Errorf("unable to patch proxy with trustedCA: %w", err)
	}
	return nil
}

// generateCertificate generates a new self-signed PEM-encoded certificate
func generateCertificate() (string, error) {
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(33),
		Subject: pkix.Name{
			Organization:  []string{"New Test Cert Org."},
			Country:       []string{"US"},
			Province:      []string{"MA"},
			Locality:      []string{"Boston"},
			StreetAddress: []string{"New Test Cert St."},
			PostalCode:    []string{"02115"},
		},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certPrivKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return "", err
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, cert, cert, &certPrivKey.PublicKey, certPrivKey)
	if err != nil {
		return "", err
	}
	certPEM := new(bytes.Buffer)
	pem.Encode(certPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})
	return certPEM.String(), nil
}

// parseWindowsEnvVars parses the Powershell output listing all environment variables with their name, value pairs
// and returns a map of ENV vars to their corresponding values.
// Sample input:
// Name  : HTTP_PROXY
// Value : http://dev:d3436c0b817f7ca8e23f7b47be49945d@10.0.1.10:3128/
// Name  : SHELL
// Value : c:\windows\system32\cmd.exe
func parseWindowsEnvVars(pwshOutput string) map[string]string {
	var valueLines []string
	var value string
	var currentVarName string
	proxyEnvVars := make(map[string]string)
	lines := strings.Split(pwshOutput, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				currentVarName = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "Value") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				valueLine := strings.TrimSpace(strings.TrimPrefix(parts[1], "Value:"))
				valueLines = []string{valueLine}
			} // case when a long ENV var value like NO_PROXY is split into multiple elements
		} else if line != "" {
			valueLines = append(valueLines, line)
		}
		if len(valueLines) > 0 {
			value = strings.Join(valueLines, "")
			value = strings.ReplaceAll(value, ";", ",")
			proxyEnvVars[currentVarName] = value
		}
	}
	return proxyEnvVars
}
