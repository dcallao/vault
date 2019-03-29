package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/builtin/logical/database/dbplugin"
	"github.com/hashicorp/vault/plugins"
	"github.com/hashicorp/vault/plugins/helper/database/credsutil"
	"github.com/hashicorp/vault/plugins/helper/database/dbutil"
)

// Elasticsearch implements dbplugin's Database interface
type Elasticsearch struct {
	logger             hclog.Logger
	credentialProducer credsutil.CredentialsProducer
	clientFactory      *clientFactory
}

func New() (interface{}, error) {
	return &Elasticsearch{
		logger: hclog.Default(), // TODO what if Vault is on Debug? Will this pick it up?
		credentialProducer: &credsutil.SQLCredentialsProducer{
			DisplayNameLen: 15,
			RoleNameLen:    15,
			UsernameLen:    100,
			Separator:      "-",
		},
		clientFactory: &clientFactory{
			clientConfig: &ClientConfig{},
		},
	}, nil
}

func Run(apiTLSConfig *api.TLSConfig) error {
	dbType, err := New()
	if err != nil {
		return err
	}
	plugins.Serve(dbType.(dbplugin.Database), apiTLSConfig)
	return nil
}

func (es *Elasticsearch) Type() (string, error) {
	return "elasticsearch", nil
}

func (es *Elasticsearch) Init(ctx context.Context, rootConfig map[string]interface{}, verifyConnection bool) (map[string]interface{}, error) {
	es.logger.Debug("initializing...")
	inboundConfig := &ClientConfig{
		Logger: es.logger,
	}

	raw, ok := rootConfig["username"]
	if !ok {
		return nil, errors.New(`"username" must be provided`)
	}
	inboundConfig.Username, ok = raw.(string)
	if !ok {
		return nil, errors.New(`"username" must be a string`)
	}

	raw, ok = rootConfig["password"]
	if !ok {
		return nil, errors.New(`"password" must be provided`)
	}
	inboundConfig.Password, ok = raw.(string)
	if !ok {
		return nil, errors.New(`"password" must be a string"`)
	}

	raw, ok = rootConfig["url"]
	if !ok {
		return nil, errors.New(`"url" must be provided`)
	}
	inboundConfig.BaseURL, ok = raw.(string)
	if !ok {
		return nil, errors.New(`"url" must be a string`)
	}

	tlsConfigInbound := false
	inboundTLSConfig := &TLSConfig{}

	if raw, ok := rootConfig["ca_cert"]; ok {
		inboundTLSConfig.CACert, ok = raw.(string)
		if !ok {
			return nil, errors.New(`"ca_cert" must be a string`)
		}
		tlsConfigInbound = true
	}
	if raw, ok := rootConfig["ca_path"]; ok {
		inboundTLSConfig.CAPath, ok = raw.(string)
		if !ok {
			return nil, errors.New(`"ca_path" must be a string`)
		}
		tlsConfigInbound = true
	}
	if raw, ok := rootConfig["client_cert"]; ok {
		inboundTLSConfig.ClientCert, ok = raw.(string)
		if !ok {
			return nil, errors.New(`"client_cert" must be a string`)
		}
		tlsConfigInbound = true
	}
	if raw, ok := rootConfig["client_key"]; ok {
		inboundTLSConfig.ClientKey, ok = raw.(string)
		if !ok {
			return nil, errors.New(`"client_key" must be a string`)
		}
		tlsConfigInbound = true
	}
	if raw, ok := rootConfig["tls_server_name"]; ok {
		inboundTLSConfig.TLSServerName, ok = raw.(string)
		if !ok {
			return nil, errors.New(`"tls_server_name" must be a string`)
		}
		tlsConfigInbound = true
	}
	if raw, ok := rootConfig["insecure"]; ok {
		inboundTLSConfig.Insecure, ok = raw.(bool)
		if !ok {
			return nil, errors.New(`"insecure" must be a bool`)
		}
		tlsConfigInbound = true
	}

	// If no TLS config was provided, the user probably doesn't want TLS.
	// We flag this to the client by leaving the TLS config pointer nil. So, we should
	// only fulfill this pointer if the user actually wants TLS.
	if tlsConfigInbound {
		inboundConfig.TLSConfig = inboundTLSConfig
	}

	// Let's always do an initial check that the client config at least _looks_ reasonable.
	inboundClient, err := NewClient(inboundConfig)
	if err != nil {
		return nil, err
	}

	if verifyConnection {
		// Whether this role is found or unfound, if we're configured correctly there will
		// be no err from the call. However, if something is misconfigured, this will yield
		// an error response, which will be described in the returned error.
		if _, err := inboundClient.GetRole(ctx.Done(), "vault-test"); err != nil {
			return nil, err
		}
	}
	es.clientFactory.UpdateConfig(inboundConfig)
	es.logger.Debug(fmt.Sprintf("successfully updated config to %s", inboundConfig))
	return nil, nil
}

func (es *Elasticsearch) CreateUser(ctx context.Context, statements dbplugin.Statements, usernameConfig dbplugin.UsernameConfig, _ time.Time) (string, string, error) {
	es.logger.Debug("creating user...")
	username, err := es.credentialProducer.GenerateUsername(usernameConfig)
	if err != nil {
		return "", "", err
	}

	password, err := es.credentialProducer.GeneratePassword()
	if err != nil {
		return "", "", err
	}

	stmt, err := newCreationStatement(statements)
	if err != nil {
		return "", "", err
	}

	user := &User{
		Password: password,
		Roles:    stmt.PreexistingRoles,
	}

	client, err := es.clientFactory.GetClient()
	if err != nil {
		return "", "", err
	}

	if len(stmt.RoleToCreate) > 0 {
		if err := client.CreateRole(ctx.Done(), username, stmt.RoleToCreate); err != nil {
			return "", "", err
		}
		es.logger.Debug("created role named %s", username)
		user.Roles = []string{username}
	}
	if err := client.CreateUser(ctx.Done(), username, user); err != nil {
		return "", "", err
	}
	es.logger.Debug(fmt.Sprintf("created user named %s", username))
	return username, password, nil
}

func (es *Elasticsearch) RenewUser(_ context.Context, _ dbplugin.Statements, _ string, _ time.Time) error {
	// This is not implemented because you can't put an expiration on roles or users. They can only be created
	// and explicitly revoked. Normally, this function would update a "VALID UNTIL" statement on a database user
	// but there's no similar need here.
	return nil
}

func (es *Elasticsearch) RevokeUser(ctx context.Context, statements dbplugin.Statements, username string) error {
	es.logger.Debug(fmt.Sprintf("revoking user %s...", username))
	stmt, err := newCreationStatement(statements)
	if err != nil {
		return err
	}

	client, err := es.clientFactory.GetClient()
	if err != nil {
		return err
	}

	var errs error
	if len(stmt.RoleToCreate) > 0 {
		// If the role already doesn't exist because it was successfully deleted on a previous
		// attempt to run this code, there will be no error, so it's harmless to try.
		if err := client.DeleteRole(ctx.Done(), username); err != nil {
			errs = multierror.Append(errs, err)
		}
		es.logger.Debug(fmt.Sprintf("deleted role named %s", username))
	}
	// Same with the user. If it was already deleted on a previous attempt, there won't be an
	// error.
	if err := client.DeleteUser(ctx.Done(), username); err != nil {
		errs = multierror.Append(errs, err)
	}
	es.logger.Debug(fmt.Sprintf("deleted user named %s", username))
	return errs
}

func (es *Elasticsearch) RotateRootCredentials(ctx context.Context, statements []string) (map[string]interface{}, error) {
	es.logger.Debug("rotating root credentials...")
	if len(statements) != 1 || statements[0] != "TRUE" {
		es.logger.Debug(`root credential rotation is not enabled; to enable it, set revocation_statements="TRUE"`)
		return nil, nil
	}
	newPassword, err := es.credentialProducer.GeneratePassword()
	if err != nil {
		return nil, err
	}
	if err := es.clientFactory.UpdatePassword(ctx.Done(), newPassword); err != nil {
		return nil, err
	}
	es.logger.Debug("password updated")
	return nil, nil
}

// This gets called after calling roles or creds.
func (es *Elasticsearch) Close() error {
	// NOOP, nothing to close.
	return nil
}

// DEPRECATED, included for backward-compatibility until removal
func (es *Elasticsearch) Initialize(ctx context.Context, config map[string]interface{}, verifyConnection bool) error {
	_, err := es.Init(ctx, config, verifyConnection)
	return err
}

func newCreationStatement(statements dbplugin.Statements) (*creationStatement, error) {
	if len(statements.Creation) == 0 {
		return nil, dbutil.ErrEmptyCreationStatement
	}
	var stmt *creationStatement
	if err := json.Unmarshal([]byte(statements.Creation[0]), stmt); err != nil {
		return nil, err
	}
	if len(stmt.PreexistingRoles) > 0 && len(stmt.RoleToCreate) > 0 {
		return nil, errors.New(`"elasticsearch_roles" and "elasticsearch_role_definition" are mutually exclusive`)
	}
	return stmt, nil
}

type creationStatement struct {
	PreexistingRoles []string               `json:"elasticsearch_roles"`
	RoleToCreate     map[string]interface{} `json:"elasticsearch_role_definition"`
}

// clientFactory prevents races because both the config endpoint and the rotate root action
// could be acting upon the password, right when the password is being read to create new
// clients for requests.
// Rather than spread the mutex's logic across all endpoints, it's safer and clearer
// to hold the synchronization within a factory that handles all the details.
// It also results in less code repetition, shorter periods of holding the lock,
// and is easier to unit test.
type clientFactory struct {
	clientConfig *ClientConfig
	mux          sync.Mutex
}

func (f *clientFactory) GetClient() (*Client, error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	return NewClient(f.clientConfig)
}

func (f *clientFactory) UpdateConfig(clientConfig *ClientConfig) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.clientConfig = clientConfig
}

func (f *clientFactory) UpdatePassword(done <-chan struct{}, newPassword string) error {
	client, err := f.GetClient()
	if err != nil {
		return err
	}
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := client.ChangePassword(done, f.clientConfig.Username, newPassword); err != nil {
		return err
	}
	f.clientConfig.Password = newPassword
	return nil
}