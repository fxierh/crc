package machine

import (
	gocontext "context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crc-org/crc/v2/pkg/crc/constants"
	"github.com/crc-org/crc/v2/pkg/crc/logging"
	"github.com/crc-org/crc/v2/pkg/crc/machine/types"
	"github.com/openshift/library-go/pkg/oauth/tokenrequest"
	"github.com/openshift/library-go/pkg/oauth/tokenrequest/challengehandlers"
	"k8s.io/apimachinery/third_party/forked/golang/netutil"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const (
	adminContext     = "crc-admin"
	developerContext = "crc-developer"
)

func updateClientCrtAndKeyToKubeconfig(clientKey, clientCrt []byte, srcKubeconfigPath, destKubeconfigPath string) error {
	cfg, err := clientcmd.LoadFromFile(srcKubeconfigPath)
	if err != nil {
		return err
	}
	var adminUserExist bool
	for username, auth := range cfg.AuthInfos {
		if username == "admin" {
			adminUserExist = true
			auth.ClientCertificateData = clientCrt
			auth.ClientKeyData = clientKey
		}
	}
	if !adminUserExist {
		return fmt.Errorf("unable to find admin user in %s", srcKubeconfigPath)
	}
	return clientcmd.WriteToFile(*cfg, destKubeconfigPath)
}

func writeKubeconfig(ip string, clusterConfig *types.ClusterConfig, ingressHTTPSPort uint) error {
	kubeconfig, cfg, err := getGlobalKubeConfig()
	if err != nil {
		return err
	}
	ca, err := certificateAuthority(clusterConfig.KubeConfig)
	if err != nil {
		return err
	}
	host, err := hostname(clusterConfig.ClusterAPI)
	if err != nil {
		return err
	}

	cfg.Clusters[host] = &api.Cluster{
		Server:                   clusterConfig.ClusterAPI,
		CertificateAuthorityData: ca,
	}

	if err := addContext(cfg, ip, clusterConfig, ca, adminContext, "kubeadmin", clusterConfig.KubeAdminPass, ingressHTTPSPort); err != nil {
		return err
	}
	if err := addContext(cfg, ip, clusterConfig, ca, developerContext, "developer", "developer", ingressHTTPSPort); err != nil {
		return err
	}

	if cfg.CurrentContext == "" {
		cfg.CurrentContext = adminContext
	}

	return clientcmd.WriteToFile(*cfg, kubeconfig)
}

func getGlobalKubeConfig() (string, *api.Config, error) {
	kubeconfig := getGlobalKubeConfigPath()
	return getKubeConfigFromFile(kubeconfig)
}

func getKubeConfigFromFile(kubeconfig string) (string, *api.Config, error) {
	config, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil && !os.IsNotExist(err) {
		return "", nil, err
	}
	if config == nil {
		config = api.NewConfig()
	}
	return kubeconfig, config, nil
}

func certificateAuthority(kubeconfigFile string) ([]byte, error) {
	builtin, err := clientcmd.LoadFromFile(kubeconfigFile)
	if err != nil {
		return nil, err
	}
	cluster, ok := builtin.Clusters["crc"]
	if !ok {
		return nil, fmt.Errorf("crc cluster not found in kubeconfig %s", kubeconfigFile)
	}
	return cluster.CertificateAuthorityData, nil
}

func adminClientCertificate(kubeconfigFile string) (string, error) {
	builtin, err := clientcmd.LoadFromFile(kubeconfigFile)
	if err != nil {
		return "", err
	}
	users := builtin.AuthInfos
	adminUser, ok := users["admin"]
	if !ok {
		return "", fmt.Errorf("admin user is not found in kubeconfig %s", kubeconfigFile)
	}
	return string(adminUser.ClientCertificateData), nil
}

// https://github.com/openshift/oc/blob/f94afb52dc8a3185b3b9eacaf92ec34d80f8708d/pkg/helpers/kubeconfig/smart_merge.go#L21
func hostname(clusterAPI string) (string, error) {
	p, err := url.Parse(clusterAPI)
	if err != nil {
		return "", err
	}
	h := netutil.CanonicalAddr(p)
	return strings.ReplaceAll(h, ".", "-"), nil
}

func addContext(cfg *api.Config, ip string, clusterConfig *types.ClusterConfig, ca []byte, context, username, password string, ingressHTTPSPort uint) error {
	host, err := hostname(clusterConfig.ClusterAPI)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	ok := roots.AppendCertsFromPEM(ca)
	if !ok {
		return fmt.Errorf("failed to parse root certificate")
	}
	restConfig := &restclient.Config{
		Proxy: clusterConfig.ProxyConfig.ProxyFunc(),
		Host:  clusterConfig.ClusterAPI,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			},
			DialContext: func(ctx gocontext.Context, network, address string) (net.Conn, error) {
				logging.Debugf("Using address: %s", address)
				hostname, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(hostname, constants.AppsDomain) {
					port = strconv.Itoa(int(ingressHTTPSPort))
				}
				dialer := net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}
				logging.Debugf("Dialing to %s:%s", ip, port)
				return dialer.Dial(network, fmt.Sprintf("%s:%s", ip, port))
			},
		},
	}
	challengeHandler := challengehandlers.NewBasicChallengeHandler(restConfig.Host, "" /* webconsoleURL */, nil /* in */, nil /* out */, nil /* passwordPrompter */, username, password)
	token, err := tokenrequest.RequestTokenWithChallengeHandlers(restConfig, challengeHandler)
	if err != nil {
		return err
	}
	cfg.AuthInfos[username] = &api.AuthInfo{
		Token: token,
	}
	cfg.Contexts[context] = &api.Context{
		Cluster:   host,
		AuthInfo:  username,
		Namespace: "default",
	}
	return nil
}

// getGlobalKubeConfigPath returns the path to the first entry in the KUBECONFIG environment variable
// or if KUBECONFIG is not set then $HOME/.kube/config
func getGlobalKubeConfigPath() string {
	pathList := filepath.SplitList(os.Getenv("KUBECONFIG"))
	if len(pathList) > 0 {
		// Tools should write to the last entry in the KUBECONFIG file instead of the first one.
		// oc cluster up also does the same.
		return pathList[len(pathList)-1]
	}
	return filepath.Join(constants.GetHomeDir(), ".kube", "config")
}

func cleanKubeconfig(input, output string) error {
	cfg, err := clientcmd.LoadFromFile(input)
	if err != nil {
		return err
	}

	var clusterNames []string
	for name, cluster := range cfg.Clusters {
		if cluster.Server == fmt.Sprintf("https://api%s:6443", constants.ClusterDomain) {
			clusterNames = append(clusterNames, name)
		}
	}
	var contextNames []string
	authNames := make(map[string]struct{})
	for name, context := range cfg.Contexts {
		if contains(clusterNames, context.Cluster) {
			contextNames = append(contextNames, name)
			authNames[context.AuthInfo] = struct{}{}
		}
	}
	// keep auth if it is shared with other contexts
	for _, context := range cfg.Contexts {
		if !contains(clusterNames, context.Cluster) {
			delete(authNames, context.AuthInfo)
		}
	}

	for _, name := range clusterNames {
		delete(cfg.Clusters, name)
	}
	for _, name := range contextNames {
		delete(cfg.Contexts, name)
		if cfg.CurrentContext == name {
			cfg.CurrentContext = ""
		}
	}
	for name := range authNames {
		delete(cfg.AuthInfos, name)
	}

	return clientcmd.WriteToFile(*cfg, output)
}

func contains(arr []string, str string) bool {
	for _, a := range arr {
		if a == str {
			return true
		}
	}
	return false
}

func mergeKubeConfigFile(kubeConfigFile string) error {
	return mergeConfigHelper(kubeConfigFile, getGlobalKubeConfigPath())
}

func mergeConfigHelper(kubeConfigFile, globalConfigFile string) error {

	globalConfigPath, globalConf, err := getKubeConfigFromFile(globalConfigFile)
	if err != nil {
		return err
	}

	currentConf, err := clientcmd.LoadFromFile(kubeConfigFile)
	if err != nil {
		return err
	}
	// Merge the currentConf to globalConfig
	for name, cluster := range currentConf.Clusters {
		globalConf.Clusters[name] = cluster
	}

	for name, authInfo := range currentConf.AuthInfos {
		globalConf.AuthInfos[name] = authInfo
	}

	for name, context := range currentConf.Contexts {
		globalConf.Contexts[name] = context
	}

	globalConf.CurrentContext = currentConf.CurrentContext
	return clientcmd.WriteToFile(*globalConf, globalConfigPath)
}
