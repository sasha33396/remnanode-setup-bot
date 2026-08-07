package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"remnanode-setup-bot/internal/certificates"
	sshclient "remnanode-setup-bot/internal/ssh"
)

const (
	xraySNIInstallDirectory = "/opt/xray-sni"
	xraySNIPort             = 9443
)

var (
	ErrInvalidXraySNIConfiguration = errors.New("invalid xray-sni configuration")
	ErrXraySNIInspectionFailed     = errors.New("xray-sni inspection failed")
	ErrXraySNIInstallationFailed   = errors.New("xray-sni installation failed")
	ErrXraySNIValidationFailed     = errors.New("xray-sni validation failed")
)

// ExternalXraySNIConfig selects an immutable source revision and the SNI
// domain derived by the workflow from host.address.
type ExternalXraySNIConfig struct {
	RepositoryURL string
	Ref           string
	SNIDomain     string
	Timeout       time.Duration
}

// ExternalXraySNIInstaller installs the external-certificate xray-sni fork.
// Certificate bytes are retained only in memory and sent over SSH stdin.
type ExternalXraySNIInstaller struct {
	runner   sshclient.CommandRunner
	config   ExternalXraySNIConfig
	material certificates.Material
	now      func() time.Time
}

func NewExternalXraySNIInstaller(runner sshclient.CommandRunner, config ExternalXraySNIConfig, material certificates.Material) (*ExternalXraySNIInstaller, error) {
	if runner == nil || !validExternalXraySNIConfig(config) {
		return nil, ErrInvalidXraySNIConfiguration
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Minute
	}
	material = material.Clone()
	if err := validateCertificateMaterial(material, config.SNIDomain, time.Now()); err != nil {
		material.Destroy()
		return nil, err
	}
	return &ExternalXraySNIInstaller{runner: runner, config: config, material: material, now: time.Now}, nil
}

// Destroy clears the adapter's retained private-key copy.
func (a *ExternalXraySNIInstaller) Destroy() { a.material.Destroy() }

func (*ExternalXraySNIInstaller) String() string {
	return "ExternalXraySNIInstaller{CertificateMaterial:[REDACTED]}"
}
func (*ExternalXraySNIInstaller) GoString() string {
	return "ExternalXraySNIInstaller{CertificateMaterial:[REDACTED]}"
}

// LogValue prevents structured logging from reflecting over retained PEM data.
func (a *ExternalXraySNIInstaller) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("repository_url", a.config.RepositoryURL),
		slog.String("ref", a.config.Ref),
		slog.String("sni_domain", a.config.SNIDomain),
		slog.String("certificate_material", "[REDACTED]"),
	)
}

func (a *ExternalXraySNIInstaller) Inspect(ctx context.Context) (Inspection, error) {
	state, err := a.inspectState(ctx)
	if err != nil {
		return Inspection{}, err
	}
	if summary := state.repairSummary(); summary != "" {
		return Inspection{Summary: summary}, nil
	}
	if err := a.validateRuntime(ctx); err != nil {
		if ctx.Err() != nil {
			return Inspection{}, ctx.Err()
		}
		return Inspection{Summary: "xray-sni runtime requires repair"}, nil
	}
	return Inspection{Satisfied: true, Summary: "xray-sni external mode is healthy"}, nil
}

func (a *ExternalXraySNIInstaller) Install(ctx context.Context) error {
	if err := validateCertificateMaterial(a.material, a.config.SNIDomain, a.now()); err != nil {
		return err
	}
	state, err := a.inspectState(ctx)
	if err != nil {
		return err
	}
	repositoryChanged := !state.RepositoryExists || !state.RemoteMatches || !state.RefMatches || !state.WorktreeClean || !state.ComposeExists
	buildRequired := repositoryChanged || !state.DeployedCommitMatches
	environmentChanged := !state.EnvironmentMatches
	certificatesChanged := !state.FullchainMatches || !state.PrivateKeyMatches
	permissionsChanged := !state.PermissionsMatch
	containerRequiresStart := !state.ContainerExists || !state.ContainerRunning

	if repositoryChanged {
		if err := a.ensureRepository(ctx); err != nil {
			return err
		}
	}
	if environmentChanged {
		if err := a.writeManagedFile(ctx, managedFile{path: xraySNIInstallDirectory + "/.env", mode: "0600", content: a.environment()}); err != nil {
			return err
		}
	}
	if certificatesChanged {
		if err := a.stageAndActivateCertificates(ctx); err != nil {
			return err
		}
	} else if permissionsChanged {
		if err := a.ensureCertificatePermissions(ctx); err != nil {
			return err
		}
	}
	if err := a.composeConfig(ctx); err != nil {
		return err
	}
	switch {
	case buildRequired:
		if err := a.runSafe(ctx, "# xray-sni:compose-build\ncd /opt/xray-sni && docker compose up -d --build --remove-orphans", ErrXraySNIInstallationFailed); err != nil {
			return err
		}
		if err := a.recordDeployedCommit(ctx); err != nil {
			return err
		}
	case environmentChanged || containerRequiresStart:
		if err := a.runSafe(ctx, "# xray-sni:compose-up\ncd /opt/xray-sni && docker compose up -d --remove-orphans", ErrXraySNIInstallationFailed); err != nil {
			return err
		}
	case certificatesChanged:
		if err := a.reload(ctx); err != nil {
			return err
		}
	case !permissionsChanged:
		if err := a.reload(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *ExternalXraySNIInstaller) Validate(ctx context.Context) error {
	state, err := a.inspectState(ctx)
	if err != nil || state.repairSummary() != "" {
		return ErrXraySNIValidationFailed
	}
	if err := a.validateRuntime(ctx); err != nil {
		return err
	}
	return a.cleanupPreviousCertificates(ctx)
}

func (a *ExternalXraySNIInstaller) environment() []byte {
	return []byte(fmt.Sprintf("TLS_MODE=external\nSNI_DOMAIN=%s\nSNI_PORT=%d\n", a.config.SNIDomain, xraySNIPort))
}

type xraySNIState struct {
	RepositoryExists      bool
	RemoteMatches         bool
	RefMatches            bool
	WorktreeClean         bool
	ComposeExists         bool
	DeployedCommitMatches bool
	EnvironmentMatches    bool
	FullchainMatches      bool
	PrivateKeyMatches     bool
	PermissionsMatch      bool
	ContainerExists       bool
	ContainerRunning      bool
}

func (s xraySNIState) repairSummary() string {
	switch {
	case !s.RepositoryExists:
		return "xray-sni repository is not installed"
	case !s.RemoteMatches || !s.RefMatches || !s.WorktreeClean:
		return "xray-sni repository revision requires update"
	case !s.ComposeExists:
		return "xray-sni Compose project is incomplete"
	case !s.DeployedCommitMatches:
		return "xray-sni container image requires a pinned rebuild"
	case !s.EnvironmentMatches:
		return "xray-sni external mode configuration requires update"
	case !s.FullchainMatches || !s.PrivateKeyMatches:
		return "xray-sni certificate material requires update"
	case !s.PermissionsMatch:
		return "xray-sni certificate permissions require repair"
	case !s.ContainerExists || !s.ContainerRunning:
		return "xray-sni container requires start or repair"
	default:
		return ""
	}
}

func (a *ExternalXraySNIInstaller) inspectState(ctx context.Context) (xraySNIState, error) {
	repositoryURL, _ := shellQuote(a.config.RepositoryURL)
	ref, _ := shellQuote(a.config.Ref)
	environmentHash := hashBytes(a.environment())
	fullchainHash := hashBytes(a.material.FullchainPEM)
	privateKeyHash := hashBytes(a.material.PrivateKeyPEM)
	command := fmt.Sprintf(`# xray-sni:inspect
directory=/opt/xray-sni
repository_exists=false
remote_matches=false
ref_matches=false
worktree_clean=false
compose_exists=false
deployed_commit_matches=false
environment_matches=false
fullchain_matches=false
private_key_matches=false
permissions_match=false
container_exists=false
container_running=false
if [ -d "$directory/.git" ]; then
    repository_exists=true
    [ "$(git -C "$directory" remote get-url origin 2>/dev/null)" = %s ] && remote_matches=true
    expected=$(git -C "$directory" rev-parse --verify %s'^{commit}' 2>/dev/null || true)
    actual=$(git -C "$directory" rev-parse --verify HEAD 2>/dev/null || true)
    [ -n "$expected" ] && [ "$actual" = "$expected" ] && ref_matches=true
    git -C "$directory" diff --quiet && git -C "$directory" diff --cached --quiet && worktree_clean=true
fi
[ -f "$directory/docker-compose.yml" ] && [ -f "$directory/Caddyfile.external" ] && [ -f "$directory/Dockerfile" ] && compose_exists=true
[ -n "$actual" ] && [ "$(cat "$directory/.deployed-commit" 2>/dev/null)" = "$actual" ] && deployed_commit_matches=true
[ "$(sha256sum "$directory/.env" 2>/dev/null | cut -d' ' -f1)" = %s ] && environment_matches=true
[ "$(sha256sum "$directory/certs/fullchain.pem" 2>/dev/null | cut -d' ' -f1)" = %s ] && fullchain_matches=true
[ "$(sha256sum "$directory/certs/privkey.pem" 2>/dev/null | cut -d' ' -f1)" = %s ] && private_key_matches=true
[ "$(stat -c '%%U:%%G:%%a' "$directory/certs" 2>/dev/null)" = 'root:root:755' ] && [ "$(stat -c '%%U:%%G:%%a' "$directory/certs/fullchain.pem" 2>/dev/null)" = 'root:root:644' ] && [ "$(stat -c '%%U:%%G:%%a' "$directory/certs/privkey.pem" 2>/dev/null)" = 'root:root:600' ] && permissions_match=true
docker inspect snisite >/dev/null 2>&1 && container_exists=true
[ "$(docker inspect -f '{{.State.Running}}' snisite 2>/dev/null)" = true ] && container_running=true
printf 'repository_exists=%%s\nremote_matches=%%s\nref_matches=%%s\nworktree_clean=%%s\ncompose_exists=%%s\ndeployed_commit_matches=%%s\nenvironment_matches=%%s\nfullchain_matches=%%s\nprivate_key_matches=%%s\npermissions_match=%%s\ncontainer_exists=%%s\ncontainer_running=%%s\n' "$repository_exists" "$remote_matches" "$ref_matches" "$worktree_clean" "$compose_exists" "$deployed_commit_matches" "$environment_matches" "$fullchain_matches" "$private_key_matches" "$permissions_match" "$container_exists" "$container_running"`, repositoryURL, ref, environmentHash, fullchainHash, privateKeyHash)
	result, err := a.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: a.config.Timeout})
	if err != nil {
		if ctx.Err() != nil {
			return xraySNIState{}, ctx.Err()
		}
		return xraySNIState{}, ErrXraySNIInspectionFailed
	}
	values, err := parseKeyValues(result.Stdout)
	if err != nil {
		return xraySNIState{}, ErrXraySNIInspectionFailed
	}
	parse := func(name string) (bool, error) { return strconv.ParseBool(values[name]) }
	fields := []string{"repository_exists", "remote_matches", "ref_matches", "worktree_clean", "compose_exists", "deployed_commit_matches", "environment_matches", "fullchain_matches", "private_key_matches", "permissions_match", "container_exists", "container_running"}
	parsed := make([]bool, len(fields))
	for index, field := range fields {
		parsed[index], err = parse(field)
		if err != nil {
			return xraySNIState{}, ErrXraySNIInspectionFailed
		}
	}
	return xraySNIState{
		RepositoryExists: parsed[0], RemoteMatches: parsed[1], RefMatches: parsed[2], WorktreeClean: parsed[3],
		ComposeExists: parsed[4], DeployedCommitMatches: parsed[5], EnvironmentMatches: parsed[6],
		FullchainMatches: parsed[7], PrivateKeyMatches: parsed[8], PermissionsMatch: parsed[9],
		ContainerExists: parsed[10], ContainerRunning: parsed[11],
	}, nil
}

func (a *ExternalXraySNIInstaller) ensureRepository(ctx context.Context) error {
	repositoryURL, _ := shellQuote(a.config.RepositoryURL)
	ref, _ := shellQuote(a.config.Ref)
	command := fmt.Sprintf(`# xray-sni:repository
set -eu
repository=%s
ref=%s
directory=/opt/xray-sni
install -d -m 0755 /opt
if [ ! -e "$directory" ]; then
    git clone --no-checkout -- "$repository" "$directory"
elif [ ! -d "$directory/.git" ]; then
    exit 2
fi
current_remote=$(git -C "$directory" remote get-url origin)
if [ "$current_remote" != "$repository" ]; then git -C "$directory" remote set-url origin "$repository"; fi
case "$ref" in
    [0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F])
        git -C "$directory" fetch --force origin "$ref"
        expected="$ref"
        ;;
    *)
        git -C "$directory" fetch --force origin "refs/tags/$ref:refs/tags/$ref"
        expected=$(git -C "$directory" rev-parse --verify "refs/tags/$ref^{commit}")
        ;;
esac
git -C "$directory" checkout --detach --force "$expected"`, repositoryURL, ref)
	return a.runSafe(ctx, command, ErrXraySNIInstallationFailed)
}

func (a *ExternalXraySNIInstaller) writeManagedFile(ctx context.Context, file managedFile) error {
	remote := &remoteStage{runner: a.runner, timeout: a.config.Timeout}
	if err := remote.writeFile(ctx, file); err != nil {
		return ErrXraySNIInstallationFailed
	}
	return nil
}

func (a *ExternalXraySNIInstaller) stageAndActivateCertificates(ctx context.Context) error {
	if err := a.runSafe(ctx, "# xray-sni:certificate-directory\ninstall -d -o root -g root -m 0755 /opt/xray-sni/certs", ErrXraySNIInstallationFailed); err != nil {
		return err
	}
	if err := a.writeManagedFile(ctx, managedFile{path: xraySNIInstallDirectory + "/certs/fullchain.pem.new", mode: "0644", content: a.material.FullchainPEM}); err != nil {
		return err
	}
	if err := a.writeManagedFile(ctx, managedFile{path: xraySNIInstallDirectory + "/certs/privkey.pem.new", mode: "0600", content: a.material.PrivateKeyPEM}); err != nil {
		return err
	}
	command := `# xray-sni:activate-certificates
set -eu
cd /opt/xray-sni/certs
test -f fullchain.pem.new
test -f privkey.pem.new
backup=.certificate-previous
test ! -e "$backup"
install -d -o root -g root -m 0700 "$backup"
had_fullchain=false
had_private_key=false
if [ -f fullchain.pem ]; then mv fullchain.pem "$backup/fullchain.pem"; had_fullchain=true; fi
if [ -f privkey.pem ]; then mv privkey.pem "$backup/privkey.pem"; had_private_key=true; fi
if mv -f fullchain.pem.new fullchain.pem && mv -f privkey.pem.new privkey.pem && chown root:root fullchain.pem privkey.pem && chmod 0644 fullchain.pem && chmod 0600 privkey.pem; then
    true
else
    if [ "$had_fullchain" = true ]; then mv -f "$backup/fullchain.pem" fullchain.pem; else rm -f fullchain.pem; fi
    if [ "$had_private_key" = true ]; then mv -f "$backup/privkey.pem" privkey.pem; else rm -f privkey.pem; fi
    rm -f "$backup/fullchain.pem" "$backup/privkey.pem" fullchain.pem.new privkey.pem.new
    rmdir "$backup"
    exit 1
fi`
	return a.runSafe(ctx, command, ErrXraySNIInstallationFailed)
}

func (a *ExternalXraySNIInstaller) ensureCertificatePermissions(ctx context.Context) error {
	command := "# xray-sni:certificate-permissions\nchown root:root /opt/xray-sni/certs /opt/xray-sni/certs/fullchain.pem /opt/xray-sni/certs/privkey.pem && chmod 0755 /opt/xray-sni/certs && chmod 0644 /opt/xray-sni/certs/fullchain.pem && chmod 0600 /opt/xray-sni/certs/privkey.pem"
	return a.runSafe(ctx, command, ErrXraySNIInstallationFailed)
}

func (a *ExternalXraySNIInstaller) cleanupPreviousCertificates(ctx context.Context) error {
	command := `# xray-sni:cleanup-previous
directory=/opt/xray-sni/certs/.certificate-previous
if [ -d "$directory" ]; then
    rm -f "$directory/fullchain.pem" "$directory/privkey.pem"
    rmdir "$directory"
fi`
	return a.runSafe(ctx, command, ErrXraySNIValidationFailed)
}

func (a *ExternalXraySNIInstaller) composeConfig(ctx context.Context) error {
	return a.runSafe(ctx, "# xray-sni:compose-config\ncd /opt/xray-sni && docker compose config >/dev/null", ErrXraySNIValidationFailed)
}

func (a *ExternalXraySNIInstaller) recordDeployedCommit(ctx context.Context) error {
	command := `# xray-sni:record-deployed-commit
set -eu
cd /opt/xray-sni
temporary=$(mktemp .deployed-commit.XXXXXX)
trap 'rm -f "$temporary"' EXIT
git rev-parse HEAD > "$temporary"
chmod 0644 "$temporary"
mv -f "$temporary" .deployed-commit
trap - EXIT`
	return a.runSafe(ctx, command, ErrXraySNIInstallationFailed)
}

func (a *ExternalXraySNIInstaller) reload(ctx context.Context) error {
	return a.runSafe(ctx, "# xray-sni:reload\ndocker exec snisite caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile", ErrXraySNIValidationFailed)
}

func (a *ExternalXraySNIInstaller) validateRuntime(ctx context.Context) error {
	commands := []string{
		"# xray-sni:compose-config\ncd /opt/xray-sni && docker compose config >/dev/null",
		"# xray-sni:container\ntest \"$(docker inspect -f '{{.State.Running}}' snisite 2>/dev/null)\" = true",
		"# xray-sni:caddy-validate\ndocker exec snisite caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile",
	}
	commands = append(commands, xraySNIHealthcheckCommand(a.config.SNIDomain))
	for _, command := range commands {
		if err := a.runSafe(ctx, command, ErrXraySNIValidationFailed); err != nil {
			return err
		}
	}
	return nil
}

// xraySNIHealthcheckCommand isolates the VPS-local TLS smoke test so its exact
// transport can be adjusted after real-server validation without redesigning
// installation or certificate activation.
func xraySNIHealthcheckCommand(domain string) string {
	quotedDomain, _ := shellQuote(domain)
	return fmt.Sprintf(`# xray-sni:healthcheck
domain=%s
code=$(curl --silent --show-error --output /dev/null --write-out '%%{http_code}' --resolve "$domain:%d:127.0.0.1" --cacert /opt/xray-sni/certs/fullchain.pem "https://$domain:%d/health")
test "$code" = 200`, quotedDomain, xraySNIPort, xraySNIPort)
}

func (a *ExternalXraySNIInstaller) runSafe(ctx context.Context, command string, safeError error) error {
	_, err := a.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: a.config.Timeout})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return safeError
}

func validExternalXraySNIConfig(config ExternalXraySNIConfig) bool {
	parsed, err := url.Parse(config.RepositoryURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !validXrayPinnedRef(config.Ref) {
		return false
	}
	if !validSNIDomain(config.SNIDomain) {
		return false
	}
	return true
}

func validSNIDomain(value string) bool {
	if len(value) > 253 || strings.ContainsAny(value, "\r\n\x00") || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || !strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func validXrayPinnedRef(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "main") || strings.EqualFold(value, "master") || strings.EqualFold(value, "HEAD") || strings.HasPrefix(value, "-") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && !strings.ContainsRune("._/-", char) {
			return false
		}
	}
	return true
}

func hashBytes(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}
