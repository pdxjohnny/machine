package provision

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"

	"github.com/docker/machine/libmachine/auth"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/provision/pkgaction"
	"github.com/docker/machine/libmachine/provision/serviceaction"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/swarm"
)

var (
	ErrUnknownYumOsRelease = errors.New("unknown OS for Yum repository")

	packageListTemplate = `[docker]
name=Docker Stable Repository
baseurl=https://yum.dockerproject.org/repo/main/{{.OsRelease}}/{{.OsReleaseVersion}}
priority=1
enabled=1
gpgkey=https://yum.dockerproject.org/gpg
`
	engineConfigTemplate = `[Service]
ExecStart=/usr/bin/docker -d -H tcp://0.0.0.0:{{.DockerPort}} -H unix:///var/run/docker.sock --storage-driver {{.EngineOptions.StorageDriver}} --tlsverify --tlscacert {{.AuthOptions.CaCertRemotePath}} --tlscert {{.AuthOptions.ServerCertRemotePath}} --tlskey {{.AuthOptions.ServerKeyRemotePath}} {{ range .EngineOptions.Labels }}--label {{.}} {{ end }}{{ range .EngineOptions.InsecureRegistry }}--insecure-registry {{.}} {{ end }}{{ range .EngineOptions.RegistryMirror }}--registry-mirror {{.}} {{ end }}{{ range .EngineOptions.ArbitraryFlags }}--{{.}} {{ end }}
MountFlags=slave
LimitNOFILE=1048576
LimitNPROC=1048576
LimitCORE=infinity
Environment={{range .EngineOptions.Env}}{{ printf "%q" . }} {{end}}
`
)

type PackageListInfo struct {
	OsRelease        string
	OsReleaseVersion string
}

func init() {
	Register("RedHat", &RegisteredProvisioner{
		New: NewRedHatProvisioner,
	})
}

func NewRedHatProvisioner(d drivers.Driver) Provisioner {
	return &RedHatProvisioner{
		GenericProvisioner: GenericProvisioner{
			DockerOptionsDir:  "/etc/docker",
			DaemonOptionsFile: "/etc/systemd/system/docker.service",
			OsReleaseId:       "rhel",
			Packages: []string{
				"curl",
			},
			Driver: d,
		},
	}
}

type RedHatProvisioner struct {
	GenericProvisioner
}

func (provisioner *RedHatProvisioner) SSHCommand(args string) (string, error) {
	client, err := drivers.GetSSHClientFromDriver(provisioner.Driver)
	if err != nil {
		return "", err
	}

	// redhat needs "-t" for tty allocation on ssh therefore we check for the
	// external client and add as needed.
	// Note: CentOS 7.0 needs multiple "-tt" to force tty allocation when ssh has
	// no local tty.
	switch c := client.(type) {
	case ssh.ExternalClient:
		c.BaseArgs = append(c.BaseArgs, "-tt")
		client = c
	case ssh.NativeClient:
		return c.OutputWithPty(args)
	}

	return client.Output(args)
}

func (provisioner *RedHatProvisioner) SetHostname(hostname string) error {
	command := "sh -c 'hostname %s && echo %q | tee /etc/hostname'"
	command = provisioner.Driver.SSHSudo(command)
	if _, err := provisioner.SSHCommand(fmt.Sprintf(
		command,
		hostname,
		hostname,
	)); err != nil {
		return err
	}

	// ubuntu/debian use 127.0.1.1 for non "localhost" loopback hostnames: https://www.debian.org/doc/manuals/debian-reference/ch05.en.html#_the_hostname_resolution
	if_then := "sh -c \"sed -i 's/^127.0.1.1.*/127.0.1.1 %s/g' /etc/hosts\""
	if_else := "sh -c \"echo '127.0.1.1 %s' | tee -a /etc/hosts\""
	if_then = provisioner.Driver.SSHSudo(if_then)
	if_else = provisioner.Driver.SSHSudo(if_else)
	command = fmt.Sprintf(
		"if grep -xq 127.0.1.1.* /etc/hosts; then %s; else %s; fi",
		if_then,
		if_else,
	)
	if _, err := provisioner.SSHCommand(fmt.Sprintf(
		command,
		hostname,
		hostname,
	)); err != nil {
		return err
	}

	return nil
}

func (provisioner *RedHatProvisioner) Service(name string, action serviceaction.ServiceAction) error {
	reloadDaemon := false
	switch action {
	case serviceaction.Start, serviceaction.Restart:
		reloadDaemon = true
	}

	// systemd needs reloaded when config changes on disk; we cannot
	// be sure exactly when it changes from the provisioner so
	// we call a reload on every restart to be safe
	if reloadDaemon {
		reload_command := provisioner.Driver.SSHSudo("systemctl daemon-reload")
		if _, err := provisioner.SSHCommand(reload_command); err != nil {
			return err
		}
	}

	systemctl_command := provisioner.Driver.SSHSudo("systemctl %s %s")
	command := fmt.Sprintf(systemctl_command, action.String(), name)

	if _, err := provisioner.SSHCommand(command); err != nil {
		return err
	}

	return nil
}

func (provisioner *RedHatProvisioner) Package(name string, action pkgaction.PackageAction) error {
	var packageAction string

	switch action {
	case pkgaction.Install:
		packageAction = "install"
	case pkgaction.Remove:
		packageAction = "remove"
	case pkgaction.Upgrade:
		packageAction = "upgrade"
	}

	yum_command := provisioner.Driver.SSHSudo("yum %s -y %s")
	command := fmt.Sprintf(yum_command, packageAction, name)

	if _, err := provisioner.SSHCommand(command); err != nil {
		return err
	}

	return nil
}

func installDocker(provisioner *RedHatProvisioner) error {
	if err := provisioner.installOfficialDocker(); err != nil {
		return err
	}

	if err := provisioner.Service("docker", serviceaction.Restart); err != nil {
		return err
	}

	if err := provisioner.Service("docker", serviceaction.Enable); err != nil {
		return err
	}

	return nil
}

func (provisioner *RedHatProvisioner) installOfficialDocker() error {
	log.Debug("installing docker")

	if err := provisioner.ConfigurePackageList(); err != nil {
		return err
	}

	engine_install_command := provisioner.Driver.SSHSudo("yum install -y docker-engine")
	if _, err := provisioner.SSHCommand(engine_install_command); err != nil {
		return err
	}

	return nil
}

func (provisioner *RedHatProvisioner) dockerDaemonResponding() bool {
	docker_version_command := provisioner.Driver.SSHSudo("docker version")
	if _, err := provisioner.SSHCommand(docker_version_command); err != nil {
		log.Warn("Error getting SSH command to check if the daemon is up: %s", err)
		return false
	}

	// The daemon is up if the command worked.  Carry on.
	return true
}

func (provisioner *RedHatProvisioner) Provision(swarmOptions swarm.SwarmOptions, authOptions auth.AuthOptions, engineOptions engine.EngineOptions) error {
	provisioner.SwarmOptions = swarmOptions
	provisioner.AuthOptions = authOptions
	provisioner.EngineOptions = engineOptions

	// set default storage driver for redhat
	if provisioner.EngineOptions.StorageDriver == "" {
		provisioner.EngineOptions.StorageDriver = "devicemapper"
	}

	if err := provisioner.SetHostname(provisioner.Driver.GetMachineName()); err != nil {
		return err
	}

	for _, pkg := range provisioner.Packages {
		log.Debugf("installing base package: name=%s", pkg)
		if err := provisioner.Package(pkg, pkgaction.Install); err != nil {
			return err
		}
	}

	// update OS -- this is needed for libdevicemapper and the docker install
	yum_update_command := provisioner.Driver.SSHSudo("yum -y update")
	if _, err := provisioner.SSHCommand(yum_update_command); err != nil {
		return err
	}

	// install docker
	if err := installDocker(provisioner); err != nil {
		return err
	}

	if err := mcnutils.WaitFor(provisioner.dockerDaemonResponding); err != nil {
		return err
	}

	if err := makeDockerOptionsDir(provisioner); err != nil {
		return err
	}

	provisioner.AuthOptions = setRemoteAuthOptions(provisioner)

	if err := ConfigureAuth(provisioner); err != nil {
		return err
	}

	if err := configureSwarm(provisioner, swarmOptions, provisioner.AuthOptions); err != nil {
		return err
	}

	return nil
}

func (provisioner *RedHatProvisioner) GenerateDockerOptions(dockerPort int) (*DockerOptions, error) {
	var (
		engineCfg  bytes.Buffer
		configPath = provisioner.DaemonOptionsFile
	)

	driverNameLabel := fmt.Sprintf("provider=%s", provisioner.Driver.DriverName())
	provisioner.EngineOptions.Labels = append(provisioner.EngineOptions.Labels, driverNameLabel)

	// systemd / redhat will not load options if they are on newlines
	// instead, it just continues with a different set of options; yeah...
	t, err := template.New("engineConfig").Parse(engineConfigTemplate)
	if err != nil {
		return nil, err
	}

	engineConfigContext := EngineConfigContext{
		DockerPort:       dockerPort,
		AuthOptions:      provisioner.AuthOptions,
		EngineOptions:    provisioner.EngineOptions,
		DockerOptionsDir: provisioner.DockerOptionsDir,
	}

	t.Execute(&engineCfg, engineConfigContext)

	daemonOptsDir := configPath
	return &DockerOptions{
		EngineOptions:     engineCfg.String(),
		EngineOptionsPath: daemonOptsDir,
	}, nil
}

func generateYumRepoList(provisioner Provisioner) (*bytes.Buffer, error) {
	packageListInfo := &PackageListInfo{}

	releaseInfo, err := provisioner.GetOsReleaseInfo()
	if err != nil {
		return nil, err
	}

	switch releaseInfo.Id {
	case "rhel", "centos":
		// rhel and centos both use the "centos" repo
		packageListInfo.OsRelease = "centos"
		packageListInfo.OsReleaseVersion = "7"
	case "fedora":
		packageListInfo.OsRelease = "fedora"
		packageListInfo.OsReleaseVersion = "22"
	default:
		return nil, ErrUnknownYumOsRelease
	}

	t, err := template.New("packageList").Parse(packageListTemplate)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	if err := t.Execute(&buf, packageListInfo); err != nil {
		return nil, err
	}

	return &buf, nil
}

func (provisioner *RedHatProvisioner) ConfigurePackageList() error {
	buf, err := generateYumRepoList(provisioner)
	if err != nil {
		return err
	}

	// we cannot use %q here as it combines the newlines in the formatting
	// on transport causing yum to not use the repo
	packageCmd := provisioner.Driver.SSHSudo("sh -c 'echo %q | sudo tee /etc/yum.repos.d/docker.repo'")
	packageCmd = fmt.Sprintf(packageCmd, buf.String())
	if _, err := provisioner.SSHCommand(packageCmd); err != nil {
		return err
	}

	return nil
}
