package config

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/gbrlsnchs/jwt/v3"

	"emperror.dev/errors"
	"github.com/acobaugh/osrelease"
	"github.com/apex/log"
	"github.com/creasty/defaults"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v2"

	"github.com/pelican-dev/wings/system"
)

const DefaultLocation = "/etc/pelican/config.yml"

// DefaultTLSConfig sets sane defaults to use when configuring the internal
// webserver to listen for public connections.
//
// @see https://blog.cloudflare.com/exposing-go-on-the-internet
var DefaultTLSConfig = &tls.Config{
	NextProtos: []string{"h2", "http/1.1"},
	CipherSuites: []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	},
	PreferServerCipherSuites: true,
	MinVersion:               tls.VersionTLS12,
	MaxVersion:               tls.VersionTLS13,
	CurvePreferences:         []tls.CurveID{tls.X25519, tls.CurveP256},
}

var (
	mu            sync.RWMutex
	_config       *Configuration
	_jwtAlgo      *jwt.HMACSHA
	_debugViaFlag bool
)

// Locker specific to writing the configuration to the disk, this happens
// in areas that might already be locked, so we don't want to crash the process.
var _writeLock sync.Mutex

// SftpConfiguration defines the configuration of the internal SFTP server.
type SftpConfiguration struct {
	// The bind address of the SFTP server.
	Address string `default:"0.0.0.0" json:"bind_address" yaml:"bind_address"`
	// The bind port of the SFTP server.
	Port int `default:"2022" json:"bind_port" yaml:"bind_port"`
	// If set to true, no write actions will be allowed on the SFTP server.
	ReadOnly bool `default:"false" yaml:"read_only"`
}

// ApiConfiguration defines the configuration for the internal API that is
// exposed by the Wings webserver.
type ApiConfiguration struct {
	// The interface that the internal webserver should bind to.
	Host string `default:"0.0.0.0" yaml:"host"`

	// The port that the internal webserver should bind to.
	Port int `default:"8080" yaml:"port"`

	// SSL configuration for the daemon.
	Ssl struct {
		Enabled         bool   `json:"enabled" yaml:"enabled"`
		CertificateFile string `json:"cert" yaml:"cert"`
		KeyFile         string `json:"key" yaml:"key"`
	}

	// Determines if functionality for allowing remote download of files into server directories
	// is enabled on this instance. If set to "true" remote downloads will not be possible for
	// servers.
	DisableRemoteDownload bool `json:"-" yaml:"disable_remote_download"`

	// The maximum size for files uploaded through the Panel in MiB.
	UploadLimit int64 `default:"100" json:"upload_limit" yaml:"upload_limit"`

	// A list of IP address of proxies that may send a X-Forwarded-For header to set the true clients IP
	TrustedProxies []string `json:"trusted_proxies" yaml:"trusted_proxies"`
}

// RemoteQueryConfiguration defines the configuration settings for remote requests
// from Wings to the Panel.
type RemoteQueryConfiguration struct {
	// The amount of time in seconds that Wings should allow for a request to the Panel API
	// to complete. If this time passes the request will be marked as failed. If your requests
	// are taking longer than 30 seconds to complete it is likely a performance issue that
	// should be resolved on the Panel, and not something that should be resolved by upping this
	// number.
	Timeout int `default:"30" yaml:"timeout"`

	// The number of servers to load in a single request to the Panel API when booting the
	// Wings instance. A single request is initially made to the Panel to get this number
	// of servers, and then the pagination status is checked and additional requests are
	// fired off in parallel to request the remaining pages.
	//
	// It is not recommended to change this from the default as you will likely encounter
	// memory limits on your Panel instance. In the grand scheme of things 4 requests for
	// 50 servers is likely just as quick as two for 100 or one for 400, and will certainly
	// be less likely to cause performance issues on the Panel.
	BootServersPerPage int `default:"50" yaml:"boot_servers_per_page"`
}

// SystemConfiguration defines basic system configuration settings.
type SystemConfiguration struct {
	// The root directory where all of the pelican data is stored at.
	RootDirectory string `default:"/var/lib/pelican" json:"-" yaml:"root_directory"`

	// Directory where logs for server installations and other wings events are logged.
	LogDirectory string `default:"/var/log/pelican" json:"-" yaml:"log_directory"`

	// Directory where the server data is stored at.
	Data string `default:"/var/lib/pelican/volumes" json:"-" yaml:"data"`

	// Directory where server archives for transferring will be stored.
	ArchiveDirectory string `default:"/var/lib/pelican/archives" json:"-" yaml:"archive_directory"`

	// Directory where local backups will be stored on the machine.
	BackupDirectory string `default:"/var/lib/pelican/backups" json:"-" yaml:"backup_directory"`

	// TmpDirectory specifies where temporary files for pelican installation processes
	// should be created. This supports environments running docker-in-docker.
	TmpDirectory string `default:"/tmp/pelican" json:"-" yaml:"tmp_directory"`

	// The user that should own all of the server files, and be used for containers.
	Username string `default:"pelican" yaml:"username"`

	// The timezone for this Wings instance. This is detected by Wings automatically if possible,
	// and falls back to UTC if not able to be detected. If you need to set this manually, that
	// can also be done.
	//
	// This timezone value is passed into all containers created by Wings.
	Timezone string `yaml:"timezone"`

	// Definitions for the user that gets created to ensure that we can quickly access
	// this information without constantly having to do a system lookup.
	User struct {
		// Rootless controls settings related to rootless container daemons.
		Rootless struct {
			// Enabled controls whether rootless containers are enabled.
			Enabled bool `yaml:"enabled" default:"false"`
			// ContainerUID controls the UID of the user inside the container.
			// This should likely be set to 0 so the container runs as the user
			// running Wings.
			ContainerUID int `yaml:"container_uid" default:"0"`
			// ContainerGID controls the GID of the user inside the container.
			// This should likely be set to 0 so the container runs as the user
			// running Wings.
			ContainerGID int `yaml:"container_gid" default:"0"`
		} `yaml:"rootless"`

		Uid int `yaml:"uid"`
		Gid int `yaml:"gid"`

		// Passwd controls weather a passwd file is mounted in the container
		// at /etc/passwd to resolve missing user issues
		Passwd     bool   `json:"mount_passwd" yaml:"mount_passwd" default:"true"`
		PasswdFile string `json:"passwd_file" yaml:"passwd_file" default:"/etc/pelican/passwd"`
	} `yaml:"user"`

	// The amount of time in seconds that can elapse before a server's disk space calculation is
	// considered stale and a re-check should occur. DANGER: setting this value too low can seriously
	// impact system performance and cause massive I/O bottlenecks and high CPU usage for the Wings
	// process.
	//
	// Set to 0 to disable disk checking entirely. This will always return 0 for the disk space used
	// by a server and should only be set in extreme scenarios where performance is critical and
	// disk usage is not a concern.
	DiskCheckInterval int64 `default:"150" yaml:"disk_check_interval"`

	// ActivitySendInterval is the amount of time that should ellapse between aggregated server activity
	// being sent to the Panel. By default this will send activity collected over the last minute. Keep
	// in mind that only a fixed number of activity log entries, defined by ActivitySendCount, will be sent
	// in each run.
	ActivitySendInterval int `default:"60" yaml:"activity_send_interval"`

	// ActivitySendCount is the number of activity events to send per batch.
	ActivitySendCount int `default:"100" yaml:"activity_send_count"`

	// If set to true, file permissions for a server will be checked when the process is
	// booted. This can cause boot delays if the server has a large amount of files. In most
	// cases disabling this should not have any major impact unless external processes are
	// frequently modifying a servers' files.
	CheckPermissionsOnBoot bool `default:"true" yaml:"check_permissions_on_boot"`

	// If set to false Wings will not attempt to write a log rotate configuration to the disk
	// when it boots and one is not detected.
	EnableLogRotate bool `default:"true" yaml:"enable_log_rotate"`

	// The number of lines to send when a server connects to the websocket.
	WebsocketLogCount int `default:"150" yaml:"websocket_log_count"`

	Sftp SftpConfiguration `yaml:"sftp"`

	CrashDetection CrashDetection `yaml:"crash_detection"`

	// The ammount of lines the activity logs should log on server crash
	CrashActivityLogLines int `default:"2" yaml:"crash_detection_activity_lines"`

	Backups Backups `yaml:"backups"`

	Transfers Transfers `yaml:"transfers"`

	OpenatMode string `default:"auto" yaml:"openat_mode"`
}

type CrashDetection struct {
	// CrashDetectionEnabled sets if crash detection is enabled globally for all servers on this node.
	CrashDetectionEnabled bool `default:"true" yaml:"enabled"`

	// Determines if Wings should detect a server that stops with a normal exit code of
	// "0" as being crashed if the process stopped without any Wings interaction. E.g.
	// the user did not press the stop button, but the process stopped cleanly.
	DetectCleanExitAsCrash bool `default:"true" yaml:"detect_clean_exit_as_crash"`

	// Timeout specifies the timeout between crashes that will not cause the server
	// to be automatically restarted, this value is used to prevent servers from
	// becoming stuck in a boot-loop after multiple consecutive crashes.
	Timeout int `default:"60" json:"timeout"`
}

type Backups struct {
	// WriteLimit imposes a Disk I/O write limit on backups to the disk, this affects all
	// backup drivers as the archiver must first write the file to the disk in order to
	// upload it to any external storage provider.
	//
	// If the value is less than 1, the write speed is unlimited,
	// if the value is greater than 0, the write speed is the value in MiB/s.
	//
	// Defaults to 0 (unlimited)
	WriteLimit int `default:"0" yaml:"write_limit"`

	// CompressionLevel determines how much backups created by wings should be compressed.
	//
	// "none" -> no compression will be applied
	// "best_speed" -> uses gzip level 1 for fast speed
	// "best_compression" -> uses gzip level 9 for minimal disk space useage
	//
	// Defaults to "best_speed" (level 1)
	CompressionLevel string `default:"best_speed" yaml:"compression_level"`

	// RemoveBackupsOnServerDelete deletes backups associated with a server when the server is deleted
	RemoveBackupsOnServerDelete bool `default:"true" yaml:"remove_backups_on_server_delete"`
}

type Transfers struct {
	// DownloadLimit imposes a Network I/O read limit when downloading a transfer archive.
	//
	// If the value is less than 1, the write speed is unlimited,
	// if the value is greater than 0, the write speed is the value in MiB/s.
	//
	// Defaults to 0 (unlimited)
	DownloadLimit int `default:"0" yaml:"download_limit"`
}

type ConsoleThrottles struct {
	// Whether or not the throttler is enabled for this instance.
	Enabled bool `json:"enabled" yaml:"enabled" default:"true"`

	// The total number of lines that can be output in a given Period period before
	// a warning is triggered and counted against the server.
	Lines uint64 `json:"lines" yaml:"lines" default:"2000"`

	// The amount of time after which the number of lines processed is reset to 0. This runs in
	// a constant loop and is not affected by the current console output volumes. By default, this
	// will reset the processed line count back to 0 every 100ms.
	Period uint64 `json:"line_reset_interval" yaml:"line_reset_interval" default:"100"`
}

type Token struct {
	ID    string
	Token string
}

type Configuration struct {
	Token Token `json:"-" yaml:"-"`

	// The location from which this configuration instance was instantiated.
	path string

	// Determines if wings should be running in debug mode. This value is ignored
	// if the debug flag is passed through the command line arguments.
	Debug bool

	AppName string `default:"pelican" json:"app_name" yaml:"app_name"`

	// A unique identifier for this node in the Panel.
	Uuid string

	// An identifier for the token which must be included in any requests to the panel
	// so that the token can be looked up correctly.
	AuthenticationTokenId string `json:"token_id" yaml:"token_id"`

	// The token used when performing operations. Requests to this instance must
	// validate against it.
	AuthenticationToken string `json:"token" yaml:"token"`

	Api    ApiConfiguration    `json:"api" yaml:"api"`
	System SystemConfiguration `json:"system" yaml:"system"`
	Docker DockerConfiguration `json:"docker" yaml:"docker"`

	// Defines internal throttling configurations for server processes to prevent
	// someone from running an endless loop that spams data to logs.
	Throttles ConsoleThrottles

	// The location where the panel is running that this daemon should connect to
	// to collect data and send events.
	PanelLocation string                   `json:"-" yaml:"remote"`
	RemoteQuery   RemoteQueryConfiguration `json:"remote_query" yaml:"remote_query"`

	// AllowedMounts is a list of allowed host-system mount points.
	// This is required to have the "Server Mounts" feature work properly.
	AllowedMounts []string `json:"-" yaml:"allowed_mounts"`

	SearchRecursion SearchRecursion `yaml:"Search"`
	// BlockBaseDirMount indicates whether mounting to /home/container is blocked.
	// If true, mounting to /home/container is blocked.
	// If false, mounting to /home/container is allowed.
	BlockBaseDirMount bool `default:"true" json:"-" yaml:"BlockBaseDirMount"`

	// AllowedOrigins is a list of allowed request origins.
	// The Panel URL is automatically allowed, this is only needed for adding
	// additional origins.
	AllowedOrigins []string `json:"allowed_origins" yaml:"allowed_origins"`

	// AllowCORSPrivateNetwork sets the `Access-Control-Request-Private-Network` header which
	// allows client browsers to make requests to internal IP addresses over HTTP.  This setting
	// is only required by users running Wings without SSL certificates and using internal IP
	// addresses in order to connect. Most users should NOT enable this setting.
	AllowCORSPrivateNetwork bool `json:"allow_cors_private_network" yaml:"allow_cors_private_network"`

	// IgnorePanelConfigUpdates causes confiuration updates that are sent by the panel to be ignored.
	IgnorePanelConfigUpdates bool `json:"ignore_panel_config_updates" yaml:"ignore_panel_config_updates"`
}

// SearchRecursion holds the configuration for directory search recursion settings.
type SearchRecursion struct {
	// BlacklistedDirs is a list of directory names that should be excluded from the recursion.
	BlacklistedDirs []string `default:"[\"node_modules\", \".git\", \".wine\", \"appcache\", \"depotcache\", \"vendor\"]" yaml:"blacklisted_dirs" json:"blacklisted_dirs"`

	// MaxRecursionDepth specifies the maximum depth for directory recursion.
	MaxRecursionDepth int `default:"8" yaml:"max_recursion_depth" json:"max_recursion_depth"`
}

// NewAtPath creates a new struct and set the path where it should be stored.
// This function does not modify the currently stored global configuration.
func NewAtPath(path string) (*Configuration, error) {
	var c Configuration
	// Configures the default values for many of the configuration options present
	// in the structs. Values set in the configuration file take priority over the
	// default values.
	if err := defaults.Set(&c); err != nil {
		return nil, err
	}
	// Track the location where we created this configuration.
	c.path = path
	return &c, nil
}

// Set the global configuration instance. This is a blocking operation such that
// anything trying to set a different configuration value, or read the configuration
// will be paused until it is complete.
func Set(c *Configuration) {
	mu.Lock()
	defer mu.Unlock()
	token := c.Token.Token
	if token == "" {
		c.Token.Token = c.AuthenticationToken
		token = c.Token.Token
	}
	if _config == nil || _config.Token.Token != token {
		_jwtAlgo = jwt.NewHS256([]byte(token))
	}
	_config = c
}

// SetDebugViaFlag tracks if the application is running in debug mode because of
// a command line flag argument. If so we do not want to store that configuration
// change to the disk.
func SetDebugViaFlag(d bool) {
	mu.Lock()
	defer mu.Unlock()
	_config.Debug = d
	_debugViaFlag = d
}

// Get returns the global configuration instance. This is a thread-safe operation
// that will block if the configuration is presently being modified.
//
// Be aware that you CANNOT make modifications to the currently stored configuration
// by modifying the struct returned by this function. The only way to make
// modifications is by using the Update() function and passing data through in
// the callback.
func Get() *Configuration {
	mu.RLock()
	// Create a copy of the struct so that all modifications made beyond this
	// point are immutable.
	//goland:noinspection GoVetCopyLock
	c := *_config
	mu.RUnlock()
	return &c
}

// Update performs an in-situ update of the global configuration object using
// a thread-safe mutex lock. This is the correct way to make modifications to
// the global configuration.
func Update(callback func(c *Configuration)) {
	mu.Lock()
	defer mu.Unlock()
	callback(_config)
}

// GetJwtAlgorithm returns the in-memory JWT algorithm.
func GetJwtAlgorithm() *jwt.HMACSHA {
	mu.RLock()
	defer mu.RUnlock()
	return _jwtAlgo
}

// WriteToDisk writes the configuration to the disk. This is a thread safe operation
// and will only allow one write at a time. Additional calls while writing are
// queued up.
func WriteToDisk(c *Configuration) error {
	_writeLock.Lock()
	defer _writeLock.Unlock()

	//goland:noinspection GoVetCopyLock
	ccopy := *c
	// If debugging is set with the flag, don't save that to the configuration file,
	// otherwise you'll always end up in debug mode.
	if _debugViaFlag {
		ccopy.Debug = false
	}
	if c.path == "" {
		return errors.New("cannot write configuration, no path defined in struct")
	}
	b, err := yaml.Marshal(&ccopy)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path, b, 0o600); err != nil {
		return err
	}
	return nil
}

// EnsurePelicanUser ensures that the Pelican core user exists on the
// system. This user will be the owner of all data in the root data directory
// and is used as the user within containers. If files are not owned by this
// user there will be issues with permissions on Docker mount points.
//
// This function IS NOT thread safe and should only be called in the main thread
// when the application is booting.
func EnsurePelicanUser() error {
	sysName, err := getSystemName()
	if err != nil {
		return err
	}

	// Our way of detecting if wings is running inside of Docker.
	if sysName == "distroless" {
		_config.System.Username = system.FirstNotEmpty(os.Getenv("WINGS_USERNAME"), "pelican")
		_config.System.User.Uid = system.MustInt(system.FirstNotEmpty(os.Getenv("WINGS_UID"), "988"))
		_config.System.User.Gid = system.MustInt(system.FirstNotEmpty(os.Getenv("WINGS_GID"), "988"))
		return nil
	}

	if _config.System.User.Rootless.Enabled {
		log.Info("rootless mode is enabled, skipping user creation...")
		u, err := user.Current()
		if err != nil {
			return err
		}
		_config.System.Username = u.Username
		_config.System.User.Uid = system.MustInt(u.Uid)
		_config.System.User.Gid = system.MustInt(u.Gid)
		return nil
	}

	log.WithField("username", _config.System.Username).Info("checking for pelican system user")
	u, err := user.Lookup(_config.System.Username)
	// If an error is returned but it isn't the unknown user error just abort
	// the process entirely. If we did find a user, return it immediately.
	if err != nil {
		if _, ok := err.(user.UnknownUserError); !ok {
			return err
		}
	} else {
		_config.System.User.Uid = system.MustInt(u.Uid)
		_config.System.User.Gid = system.MustInt(u.Gid)
		return nil
	}

	command := fmt.Sprintf("useradd --system --no-create-home --shell /usr/sbin/nologin %s", _config.System.Username)
	// Alpine Linux is the only OS we currently support that doesn't work with the useradd
	// command, so in those cases we just modify the command a bit to work as expected.
	if strings.HasPrefix(sysName, "alpine") {
		command = fmt.Sprintf("adduser -S -D -H -G %[1]s -s /sbin/nologin %[1]s", _config.System.Username)
		// We have to create the group first on Alpine, so do that here before continuing on
		// to the user creation process.
		if _, err := exec.Command("addgroup", "-S", _config.System.Username).Output(); err != nil {
			return err
		}
	}

	split := strings.Split(command, " ")
	if _, err := exec.Command(split[0], split[1:]...).Output(); err != nil {
		return err
	}
	u, err = user.Lookup(_config.System.Username)
	if err != nil {
		return err
	}
	_config.System.User.Uid = system.MustInt(u.Uid)
	_config.System.User.Gid = system.MustInt(u.Gid)
	return nil
}

// FromFile reads the configuration from the provided file and stores it in the
// global singleton for this instance.
func FromFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c, err := NewAtPath(path)
	if err != nil {
		return err
	}

	if err := yaml.Unmarshal(b, c); err != nil {
		return err
	}

	c.Token = Token{
		ID:    os.Getenv("WINGS_TOKEN_ID"),
		Token: os.Getenv("WINGS_TOKEN"),
	}
	if c.Token.ID == "" {
		c.Token.ID = c.AuthenticationTokenId
	}
	if c.Token.Token == "" {
		c.Token.Token = c.AuthenticationToken
	}

	c.Token.ID, err = Expand(c.Token.ID)
	if err != nil {
		return err
	}
	c.Token.Token, err = Expand(c.Token.Token)
	if err != nil {
		return err
	}

	// Store this configuration in the global state.
	Set(c)
	return nil
}

// ConfigureDirectories ensures that all the system directories exist on the
// system. These directories are created so that only the owner can read the data,
// and no other users.
//
// This function IS NOT thread-safe.
func ConfigureDirectories() error {
	root := _config.System.RootDirectory
	log.WithField("path", root).Debug("ensuring root data directory exists")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}

	log.WithField("filepath", _config.System.User.PasswdFile).Debug("ensuring passwd file exists")
	if passwd, err := os.Create(_config.System.User.PasswdFile); err != nil {
		return err
	} else {
		// the WriteFile method returns an error if unsuccessful
		err := os.WriteFile(passwd.Name(), []byte(fmt.Sprintf("container:x:%d:%d::/home/container:/usr/sbin/nologin", _config.System.User.Uid, _config.System.User.Gid)), 0644)
		// handle this error
		if err != nil {
			// print it out
			fmt.Println(err)
		}
	}

	// There are a non-trivial number of users out there whose data directories are actually a
	// symlink to another location on the disk. If we do not resolve that final destination at this
	// point things will appear to work, but endless errors will be encountered when we try to
	// verify accessed paths since they will all end up resolving outside the expected data directory.
	//
	// For the sake of automating away as much of this as possible, see if the data directory is a
	// symlink, and if so resolve to its final real path, and then update the configuration to use
	// that.
	if d, err := filepath.EvalSymlinks(_config.System.Data); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if d != _config.System.Data {
		_config.System.Data = d
	}

	log.WithField("path", _config.System.Data).Debug("ensuring server data directory exists")
	if err := os.MkdirAll(_config.System.Data, 0o700); err != nil {
		return err
	}

	log.WithField("path", _config.System.ArchiveDirectory).Debug("ensuring archive data directory exists")
	if err := os.MkdirAll(_config.System.ArchiveDirectory, 0o700); err != nil {
		return err
	}

	log.WithField("path", _config.System.BackupDirectory).Debug("ensuring backup data directory exists")
	if err := os.MkdirAll(_config.System.BackupDirectory, 0o700); err != nil {
		return err
	}

	return nil
}

// EnableLogRotation writes a logrotate file for wings to the system logrotate
// configuration directory if one exists and a logrotate file is not found. This
// allows us to basically automate away the log rotation for most installs, but
// also enable users to make modifications on their own.
//
// This function IS NOT thread-safe.
func EnableLogRotation() error {
	if !_config.System.EnableLogRotate {
		log.Info("skipping log rotate configuration, disabled in wings config file")
		return nil
	}

	if st, err := os.Stat("/etc/logrotate.d"); err != nil && !os.IsNotExist(err) {
		return err
	} else if (err != nil && os.IsNotExist(err)) || !st.IsDir() {
		return nil
	}
	if _, err := os.Stat("/etc/logrotate.d/wings"); err == nil || !os.IsNotExist(err) {
		return err
	}

	log.Info("no log rotation configuration found: adding file now")
	// If we've gotten to this point it means the logrotate directory exists on the system
	// but there is not a file for wings already. In that case, let us write a new file to
	// it so files can be rotated easily.
	f, err := os.Create("/etc/logrotate.d/wings")
	if err != nil {
		return err
	}
	defer f.Close()

	t, err := template.New("logrotate").Parse(`{{.LogDirectory}}/wings.log {
    size 10M
    compress
    delaycompress
    dateext
    maxage 7
    missingok
    notifempty
    postrotate
        /usr/bin/systemctl kill -s HUP wings.service >/dev/null 2>&1 || true
    endscript
}`)
	if err != nil {
		return err
	}

	return errors.Wrap(t.Execute(f, _config.System), "config: failed to write logrotate to disk")
}

// GetStatesPath returns the location of the JSON file that tracks server states.
func (sc *SystemConfiguration) GetStatesPath() string {
	return path.Join(sc.RootDirectory, "/states.json")
}

// ConfigureTimezone sets the timezone data for the configuration if it is
// currently missing. If a value has been set, this functionality will only run
// to validate that the timezone being used is valid.
//
// This function IS NOT thread-safe.
func ConfigureTimezone() error {
	tz := os.Getenv("TZ")
	if _config.System.Timezone == "" && tz != "" {
		_config.System.Timezone = tz
	}
	if _config.System.Timezone == "" {
		b, err := os.ReadFile("/etc/timezone")
		if err != nil {
			if !os.IsNotExist(err) {
				return errors.WithMessage(err, "config: failed to open timezone file")
			}

			_config.System.Timezone = "UTC"
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()
			// Okay, file isn't found on this OS, we will try using timedatectl to handle this. If this
			// command fails, exit, but if it returns a value use that. If no value is returned we will
			// fall through to UTC to get Wings booted at least.
			out, err := exec.CommandContext(ctx, "timedatectl").Output()
			if err != nil {
				log.WithField("error", err).Warn("failed to execute \"timedatectl\" to determine system timezone, falling back to UTC")
				return nil
			}

			r := regexp.MustCompile(`Time zone: ([\w/]+)`)
			matches := r.FindSubmatch(out)
			if len(matches) != 2 || string(matches[1]) == "" {
				log.Warn("failed to parse timezone from \"timedatectl\" output, falling back to UTC")
				return nil
			}
			_config.System.Timezone = string(matches[1])
		} else {
			_config.System.Timezone = string(b)
		}
	}

	_config.System.Timezone = regexp.MustCompile(`(?i)[^a-z_/]+`).ReplaceAllString(_config.System.Timezone, "")
	_, err := time.LoadLocation(_config.System.Timezone)

	return errors.WithMessage(err, fmt.Sprintf("the supplied timezone %s is invalid", _config.System.Timezone))
}

// Gets the system release name.
func getSystemName() (string, error) {
	// use osrelease to get release version and ID
	release, err := osrelease.Read()
	if err != nil {
		return "", err
	}
	return release["ID"], nil
}

var (
	openat2    atomic.Bool
	openat2Set atomic.Bool
)

func UseOpenat2() bool {
	if openat2Set.Load() {
		return openat2.Load()
	}
	defer openat2Set.Store(true)

	c := Get()
	openatMode := c.System.OpenatMode
	switch openatMode {
	case "openat2":
		openat2.Store(true)
		return true
	case "openat":
		openat2.Store(false)
		return false
	default:
		fd, err := unix.Openat2(unix.AT_FDCWD, "/", &unix.OpenHow{})
		if err != nil {
			log.WithError(err).Warn("error occurred while checking for openat2 support, falling back to openat")
			openat2.Store(false)
			return false
		}
		_ = unix.Close(fd)
		openat2.Store(true)
		return true
	}
}

// Expand expands an input string by calling [os.ExpandEnv] to expand all
// environment variables, then checks if the value is prefixed with `file://`
// to support reading the value from a file.
//
// NOTE: the order of expanding environment variables first then checking if
// the value references a file is important. This behaviour allows a user to
// pass a value like `file://${CREDENTIALS_DIRECTORY}/token` to allow us to
// work with credentials loaded by systemd's `LoadCredential` (or `LoadCredentialEncrypted`)
// options without the user needing to assume the path of `CREDENTIALS_DIRECTORY`
// or use a preStart script to read the files for us.
func Expand(v string) (string, error) {
	// Expand environment variables within the string.
	//
	// NOTE: this may cause issues if the string contains `$` and doesn't intend
	// on getting expanded, however we are using this for our tokens which are
	// all alphanumeric characters only.
	v = os.ExpandEnv(v)

	// Handle files.
	const filePrefix = "file://"
	if strings.HasPrefix(v, filePrefix) {
		p := v[len(filePrefix):]

		b, err := os.ReadFile(p)
		if err != nil {
			return "", nil
		}
		v = string(bytes.TrimRight(bytes.TrimRight(b, "\r"), "\n"))
	}

	return v, nil
}
