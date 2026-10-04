package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TASK-3399 (lead ruling Q1): a delegated grant to an installed app writes
// no oauth_connections row, so it must not surface as an MCP connection
// anywhere. Every store function whose SQL reads the connection tables or
// treats token rows as chains is classified here. A new one fails until it
// is classified.
var task3399ReaderCensus = map[string]string{
	"account_claim.go::claimAccountTx":                                  censusUserRevoke,
	"app_grants.go::ListUserAppGrants":                                  censusAppPath,
	"connected_apps.go::ListUserOAuthConnections":                       censusFilters,
	"connected_apps.go::RevokeUserOAuthConnection":                      censusPerGrant,
	"install_clients.go::DeleteInstallClientTx":                         censusAppPath,
	"mcp_resume.go::CountLiveOAuthConnections":                          censusFilters,
	"oauth.go::CountActiveOAuthAccessTokens":                            censusByDesign,
	"oauth.go::DeleteAccessToken":                                       censusPerGrant,
	"oauth.go::DeleteOAuthClient":                                       censusPerGrant,
	"oauth.go::DeleteRefreshToken":                                      censusPerGrant,
	"oauth.go::OldestAccessTokenIssuedAtByRequestID":                    censusPerGrant,
	"oauth.go::queryOAuthRequestRow":                                    censusPerGrant,
	"oauth_connections.go::AddConnectionWorkspace":                      censusNoRow,
	"oauth_connections.go::AddCreatedWorkspaceIfPermitted":              censusNoRow,
	"oauth_connections.go::ConnectionWorkspaceCount":                    censusNoRow,
	"oauth_connections.go::CreateOAuthConnection":                       censusNoRow,
	"oauth_connections.go::DeleteOAuthConnection":                       censusNoRow,
	"oauth_connections.go::GetOAuthConnection":                          censusNoRow,
	"oauth_connections.go::GetOAuthConnectionAccess":                    censusNoRow,
	"oauth_connections.go::HasActiveConnectionForUser":                  censusNoRow,
	"oauth_connections.go::IsConnectionWorkspaceAllowed":                censusNoRow,
	"oauth_connections.go::IsWorkspaceCoveredForUser":                   censusNoRow,
	"oauth_connections.go::LimitConnectionToCurrentWorkspaces":          censusNoRow,
	"oauth_connections.go::ListConnectionWorkspaceSlugs":                censusNoRow,
	"oauth_connections.go::RemoveConnectionWorkspace":                   censusNoRow,
	"oauth_connections.go::RemoveConnectionWorkspaceUnlessLast":         censusNoRow,
	"oauth_connections.go::RenameConnection":                            censusNoRow,
	"oauth_connections.go::SetScopeFlags":                               censusNoRow,
	"oauth_connections.go::SetScopeFlagsGuarded":                        censusNoRow,
	"oauth_connections.go::assertRowAffected":                           censusNoRow,
	"oauth_connections.go::lockConnectionTx":                            censusNoRow,
	"oauth_connections.go::snapshotCurrentWorkspacesSQL":                censusNoRow,
	"oauth_connections_backfill.go::BackfillOAuthConnections":           censusFilters,
	"oauth_connections_backfill.go::backfillOneChain":                   censusFilters,
	"oauth_connections_backfill.go::collectGrantChainsForBackfill":      censusFilters,
	"oauth_connections_narrow.go::NarrowCurrentOnlyWildcardConnections": censusNoRow,
	"oauth_sweep.go::SweepExpiredOAuthRows":                             censusByDesign,
	"users.go::disableUserAndRevokeAccessTx":                            censusUserRevoke,
	"users.go::eraseUserTx":                                             censusUserRevoke,
}

const (
	censusNoRow      = "reads oauth_connections rows: an install chain has none, so it is untouched by construction"
	censusPerGrant   = "addressed by one signature, request id or client id: never lists a user's chains"
	censusFilters    = "lists or counts chains as connections: filters install clients (notInstallClientSQL)"
	censusAppPath    = "the app-grant path itself (bindings, the barrier, app lifecycle)"
	censusByDesign   = "counts or sweeps every token on purpose (an ops gauge, the expiry sweep)"
	censusUserRevoke = "revokes or erases every credential of one user: delegated app codes through their bindings (delegated_user_id), tokens by subject"
)

func TestTask3399_EveryConnectionReaderIsClassified(t *testing.T) {
	needles := []string{"oauth_connections", "oauth_connection_workspaces", "FROM oauth_access_tokens", "FROM oauth_refresh_tokens"}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			hit := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					v = lit.Value
				}
				for _, nd := range needles {
					if strings.Contains(v, nd) {
						hit = true
					}
				}
				return true
			})
			if hit {
				found[name+"::"+fn.Name.Name] = true
			}
		}
	}
	var missing []string
	for k := range found {
		if _, ok := task3399ReaderCensus[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		t.Errorf("%s reads connection or token-chain tables and is not classified in task3399ReaderCensus", k)
	}
	for k := range task3399ReaderCensus {
		if !found[k] {
			t.Errorf("task3399ReaderCensus names %s, which no longer reads those tables", k)
		}
	}
}
