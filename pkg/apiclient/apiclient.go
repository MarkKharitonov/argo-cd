package apiclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	oidc "github.com/coreos/go-oidc"
	jwt "github.com/dgrijalva/jwt-go"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/argoproj/argo-cd/common"
	"github.com/argoproj/argo-cd/server/account"
	"github.com/argoproj/argo-cd/server/application"
	"github.com/argoproj/argo-cd/server/cluster"
	"github.com/argoproj/argo-cd/server/project"
	"github.com/argoproj/argo-cd/server/repository"
	"github.com/argoproj/argo-cd/server/session"
	"github.com/argoproj/argo-cd/server/settings"
	"github.com/argoproj/argo-cd/server/version"
	grpc_util "github.com/argoproj/argo-cd/util/grpc"
	"github.com/argoproj/argo-cd/util/localconfig"
)

const (
	MetaDataTokenKey = "token"
	// EnvArgoCDServer is the environment variable to look for an ArgoCD server address
	EnvArgoCDServer = "ARGOCD_SERVER"
	// EnvArgoCDAuthToken is the environment variable to look for an ArgoCD auth token
	EnvArgoCDAuthToken = "ARGOCD_AUTH_TOKEN"
)

// Client defines an interface for interaction with an Argo CD server.
type Client interface {
	ClientOptions() ClientOptions
	NewConn() (*grpc.ClientConn, error)
	NewRepoClient() (*grpc.ClientConn, repository.RepositoryServiceClient, error)
	NewRepoClientOrDie() (*grpc.ClientConn, repository.RepositoryServiceClient)
	NewClusterClient() (*grpc.ClientConn, cluster.ClusterServiceClient, error)
	NewClusterClientOrDie() (*grpc.ClientConn, cluster.ClusterServiceClient)
	NewApplicationClient() (*grpc.ClientConn, application.ApplicationServiceClient, error)
	NewApplicationClientOrDie() (*grpc.ClientConn, application.ApplicationServiceClient)
	NewSessionClient() (*grpc.ClientConn, session.SessionServiceClient, error)
	NewSessionClientOrDie() (*grpc.ClientConn, session.SessionServiceClient)
	NewSettingsClient() (*grpc.ClientConn, settings.SettingsServiceClient, error)
	NewSettingsClientOrDie() (*grpc.ClientConn, settings.SettingsServiceClient)
	NewVersionClient() (*grpc.ClientConn, version.VersionServiceClient, error)
	NewVersionClientOrDie() (*grpc.ClientConn, version.VersionServiceClient)
	NewProjectClient() (*grpc.ClientConn, project.ProjectServiceClient, error)
	NewProjectClientOrDie() (*grpc.ClientConn, project.ProjectServiceClient)
	NewAccountClient() (*grpc.ClientConn, account.AccountServiceClient, error)
	NewAccountClientOrDie() (*grpc.ClientConn, account.AccountServiceClient)
}

// ClientOptions hold address, security, and other settings for the API client.
type ClientOptions struct {
	ServerAddr string
	PlainText  bool
	Insecure   bool
	CertFile   string
	AuthToken  string
	ConfigPath string
	Context    string
}

type client struct {
	ServerAddr   string
	PlainText    bool
	Insecure     bool
	CertPEMData  []byte
	AuthToken    string
	RefreshToken string
}

// NewClient creates a new API client from a set of config options.
func NewClient(opts *ClientOptions) (Client, error) {
	var c client
	localCfg, err := localconfig.ReadLocalConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	var ctxName string
	if localCfg != nil {
		configCtx, err := localCfg.ResolveContext(opts.Context)
		if err != nil {
			return nil, err
		}
		if configCtx != nil {
			c.ServerAddr = configCtx.Server.Server
			if configCtx.Server.CACertificateAuthorityData != "" {
				c.CertPEMData, err = base64.StdEncoding.DecodeString(configCtx.Server.CACertificateAuthorityData)
				if err != nil {
					return nil, err
				}
			}
			c.PlainText = configCtx.Server.PlainText
			c.Insecure = configCtx.Server.Insecure
			c.AuthToken = configCtx.User.AuthToken
			c.RefreshToken = configCtx.User.RefreshToken
			ctxName = configCtx.Name
		}
	}
	// Override server address if specified in env or CLI flag
	if serverFromEnv := os.Getenv(EnvArgoCDServer); serverFromEnv != "" {
		c.ServerAddr = serverFromEnv
	}
	if opts.ServerAddr != "" {
		c.ServerAddr = opts.ServerAddr
	}
	// Make sure we got the server address and auth token from somewhere
	if c.ServerAddr == "" {
		return nil, errors.New("ArgoCD server address unspecified")
	}
	if parts := strings.Split(c.ServerAddr, ":"); len(parts) == 1 {
		// If port is unspecified, assume the most likely port
		c.ServerAddr += ":443"
	}
	// Override auth-token if specified in env variable or CLI flag
	if authFromEnv := os.Getenv(EnvArgoCDAuthToken); authFromEnv != "" {
		c.AuthToken = authFromEnv
	}
	if opts.AuthToken != "" {
		c.AuthToken = opts.AuthToken
	}
	// Override certificate data if specified from CLI flag
	if opts.CertFile != "" {
		b, err := ioutil.ReadFile(opts.CertFile)
		if err != nil {
			return nil, err
		}
		c.CertPEMData = b
	}
	// Override insecure/plaintext options if specified from CLI
	if opts.PlainText {
		c.PlainText = true
	}
	if opts.Insecure {
		c.Insecure = true
	}
	if localCfg != nil {
		err = c.refreshAuthToken(localCfg, ctxName, opts.ConfigPath)
		if err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// refreshAuthToken refreshes a JWT auth token if it is invalid (e.g. expired)
func (c *client) refreshAuthToken(localCfg *localconfig.LocalConfig, ctxName, configPath string) error {
	configCtx, err := localCfg.ResolveContext(ctxName)
	if err != nil {
		return err
	}
	if c.RefreshToken == "" {
		// If we have no refresh token, there's no point in doing anything
		return nil
	}
	parser := &jwt.Parser{
		SkipClaimsValidation: true,
	}
	var claims jwt.StandardClaims
	_, _, err = parser.ParseUnverified(configCtx.User.AuthToken, &claims)
	if err != nil {
		return err
	}
	if claims.Valid() == nil {
		// token is still valid
		return nil
	}

	log.Debug("Auth token no longer valid. Refreshing")
	tlsConfig, err := c.tlsConfig()
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
			Proxy:           http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
	ctx := oidc.ClientContext(context.Background(), httpClient)
	var scheme string
	if c.PlainText {
		scheme = "http"
	} else {
		scheme = "https"
	}
	conf := &oauth2.Config{
		ClientID: common.ArgoCDCLIClientAppID,
		Scopes:   []string{"openid", "profile", "email", "groups", "offline_access"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  fmt.Sprintf("%s://%s%s/auth", scheme, c.ServerAddr, common.DexAPIEndpoint),
			TokenURL: fmt.Sprintf("%s://%s%s/token", scheme, c.ServerAddr, common.DexAPIEndpoint),
		},
		RedirectURL: fmt.Sprintf("%s://%s/auth/callback", scheme, c.ServerAddr),
	}
	t := &oauth2.Token{
		RefreshToken: c.RefreshToken,
	}
	token, err := conf.TokenSource(ctx, t).Token()
	if err != nil {
		return err
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return errors.New("no id_token in token response")
	}
	refreshToken, _ := token.Extra("refresh_token").(string)
	c.AuthToken = rawIDToken
	c.RefreshToken = refreshToken
	localCfg.UpsertUser(localconfig.User{
		Name:         ctxName,
		AuthToken:    c.AuthToken,
		RefreshToken: c.RefreshToken,
	})
	err = localconfig.WriteLocalConfig(*localCfg, configPath)
	if err != nil {
		return err
	}
	return nil
}

// NewClientOrDie creates a new API client from a set of config options, or fails fatally if the new client creation fails.
func NewClientOrDie(opts *ClientOptions) Client {
	client, err := NewClient(opts)
	if err != nil {
		log.Fatal(err)
	}
	return client
}

// JwtCredentials implements the gRPC credentials.Credentials interface which we is used to do
// grpc.WithPerRPCCredentials(), for authentication
type jwtCredentials struct {
	Token string
}

func (c jwtCredentials) RequireTransportSecurity() bool {
	return false
}

func (c jwtCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{
		MetaDataTokenKey: c.Token,
	}, nil
}

func (c *client) NewConn() (*grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	if !c.PlainText {
		tlsConfig, err := c.tlsConfig()
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tlsConfig)
	}
	endpointCredentials := jwtCredentials{
		Token: c.AuthToken,
	}
	return grpc_util.BlockingDial(context.Background(), "tcp", c.ServerAddr, creds, grpc.WithPerRPCCredentials(endpointCredentials))
}

func (c *client) tlsConfig() (*tls.Config, error) {
	var tlsConfig tls.Config
	if len(c.CertPEMData) > 0 {
		cp := x509.NewCertPool()
		if !cp.AppendCertsFromPEM(c.CertPEMData) {
			return nil, fmt.Errorf("credentials: failed to append certificates")
		}
		tlsConfig.RootCAs = cp
	}
	if c.Insecure {
		tlsConfig.InsecureSkipVerify = true
	}
	return &tlsConfig, nil
}

func (c *client) ClientOptions() ClientOptions {
	return ClientOptions{
		ServerAddr: c.ServerAddr,
		PlainText:  c.PlainText,
		Insecure:   c.Insecure,
		AuthToken:  c.AuthToken,
	}
}

func (c *client) NewRepoClient() (*grpc.ClientConn, repository.RepositoryServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	repoIf := repository.NewRepositoryServiceClient(conn)
	return conn, repoIf, nil
}

func (c *client) NewRepoClientOrDie() (*grpc.ClientConn, repository.RepositoryServiceClient) {
	conn, repoIf, err := c.NewRepoClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, repoIf
}

func (c *client) NewClusterClient() (*grpc.ClientConn, cluster.ClusterServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	clusterIf := cluster.NewClusterServiceClient(conn)
	return conn, clusterIf, nil
}

func (c *client) NewClusterClientOrDie() (*grpc.ClientConn, cluster.ClusterServiceClient) {
	conn, clusterIf, err := c.NewClusterClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, clusterIf
}

func (c *client) NewApplicationClient() (*grpc.ClientConn, application.ApplicationServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	appIf := application.NewApplicationServiceClient(conn)
	return conn, appIf, nil
}

func (c *client) NewApplicationClientOrDie() (*grpc.ClientConn, application.ApplicationServiceClient) {
	conn, repoIf, err := c.NewApplicationClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, repoIf
}

func (c *client) NewSessionClient() (*grpc.ClientConn, session.SessionServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	sessionIf := session.NewSessionServiceClient(conn)
	return conn, sessionIf, nil
}

func (c *client) NewSessionClientOrDie() (*grpc.ClientConn, session.SessionServiceClient) {
	conn, sessionIf, err := c.NewSessionClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, sessionIf
}

func (c *client) NewSettingsClient() (*grpc.ClientConn, settings.SettingsServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	setIf := settings.NewSettingsServiceClient(conn)
	return conn, setIf, nil
}

func (c *client) NewSettingsClientOrDie() (*grpc.ClientConn, settings.SettingsServiceClient) {
	conn, setIf, err := c.NewSettingsClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, setIf
}

func (c *client) NewVersionClient() (*grpc.ClientConn, version.VersionServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	versionIf := version.NewVersionServiceClient(conn)
	return conn, versionIf, nil
}

func (c *client) NewVersionClientOrDie() (*grpc.ClientConn, version.VersionServiceClient) {
	conn, versionIf, err := c.NewVersionClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, versionIf
}

func (c *client) NewProjectClient() (*grpc.ClientConn, project.ProjectServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	projIf := project.NewProjectServiceClient(conn)
	return conn, projIf, nil
}

func (c *client) NewProjectClientOrDie() (*grpc.ClientConn, project.ProjectServiceClient) {
	conn, projIf, err := c.NewProjectClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, projIf
}

func (c *client) NewAccountClient() (*grpc.ClientConn, account.AccountServiceClient, error) {
	conn, err := c.NewConn()
	if err != nil {
		return nil, nil, err
	}
	usrIf := account.NewAccountServiceClient(conn)
	return conn, usrIf, nil
}

func (c *client) NewAccountClientOrDie() (*grpc.ClientConn, account.AccountServiceClient) {
	conn, usrIf, err := c.NewAccountClient()
	if err != nil {
		log.Fatalf("Failed to establish connection to %s: %v", c.ServerAddr, err)
	}
	return conn, usrIf
}
