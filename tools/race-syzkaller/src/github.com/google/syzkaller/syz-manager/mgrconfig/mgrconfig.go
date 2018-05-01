// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package mgrconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/syzkaller/pkg/config"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/prog"
	_ "github.com/google/syzkaller/sys" // most mgrconfig users want targets too
	"github.com/google/syzkaller/vm"
)

type Config struct {
	Name       string // Instance name (used for identification and as GCE instance prefix)
	Target     string // Target OS/arch, e.g. "linux/arm64" or "linux/amd64/386" (amd64 OS with 386 test process)
	HTTP       string // TCP address to serve HTTP stats page (e.g. "localhost:50000")
	RPC        string // TCP address to serve RPC for fuzzer processes (optional)
	Workdir    string
	Vmlinux    string
	Kernel_Src string // kernel source directory
	Tag        string // arbitrary optional tag that is saved along with crash reports (e.g. branch/commit)
	Image      string // linux image for VMs
	SSHKey     string // ssh key for the image (may be empty for some VM types)
	SSH_User   string // ssh user ("root" by default)

	Hub_Client string
	Hub_Addr   string
	Hub_Key    string

	Email_Addrs []string // syz-manager will send crash emails to this list of emails using mailx (optional)

	Dashboard_Client string
	Dashboard_Addr   string
	Dashboard_Key    string

	Syzkaller string // path to syzkaller checkout (syz-manager will look for binaries in bin subdir)
	Procs     int    // number of parallel processes inside of every VM

	Sandbox string // type of sandbox to use during fuzzing:
	// "none": don't do anything special (has false positives, e.g. due to killing init)
	// "setuid": impersonate into user nobody (65534), default
	// "namespace": create a new namespace for fuzzer using CLONE_NEWNS/CLONE_NEWNET/CLONE_NEWPID/etc,
	//	requires building kernel with CONFIG_NAMESPACES, CONFIG_UTS_NS, CONFIG_USER_NS, CONFIG_PID_NS and CONFIG_NET_NS.

	Cover     bool // use kcov coverage (default: true)
	Leak      bool // do memory leak checking
	Reproduce bool // reproduce, localize and minimize crashers (on by default)

	Enable_Syscalls  []string
	Disable_Syscalls []string
	Suppressions     []string // don't save reports matching these regexps, but reboot VM after them
	Ignores          []string // completely ignore reports matching these regexps (don't save nor reboot)

	Type string          // VM type (qemu, kvm, local)
	VM   json.RawMessage // VM-type-specific config

	// Implementation details beyond this point.
	ParsedSuppressions []*regexp.Regexp `json:"-"`
	ParsedIgnores      []*regexp.Regexp `json:"-"`
	// Parsed Target:
	TargetOS     string `json:"-"`
	TargetArch   string `json:"-"`
	TargetVMArch string `json:"-"`
	// Syzkaller binaries that we are going to use:
	SyzFuzzerBin   string `json:"-"`
	SyzSchedBin    string `json:"-"`
	SyzExecprogBin string `json:"-"`
	SyzExecutorBin string `json:"-"`

	Mempair         string
	Mapping         string
	Disable_Mempair string
	Disable_Mapping string

	CallGraph string
	Distance  string
}

func LoadData(data []byte) (*Config, error) {
	return load(data, "")
}

func LoadFile(filename string) (*Config, error) {
	return load(nil, filename)
}

func DefaultValues() *Config {
	return &Config{
		SSH_User:  "root",
		Cover:     true,
		Reproduce: true,
		Sandbox:   "setuid",
		RPC:       ":0",
		Procs:     1,
	}
}

func load(data []byte, filename string) (*Config, error) {
	cfg := DefaultValues()
	if data != nil {
		if err := config.LoadData(data, cfg); err != nil {
			return nil, err
		}
	} else {
		if err := config.LoadFile(filename, cfg); err != nil {
			return nil, err
		}
	}

	var err error
	cfg.TargetOS, cfg.TargetVMArch, cfg.TargetArch, err = SplitTarget(cfg.Target)
	if err != nil {
		return nil, err
	}

	targetBin := func(name, arch string) string {
		exe := ""
		if cfg.TargetOS == "windows" {
			exe = ".exe"
		}
		return filepath.Join(cfg.Syzkaller, "bin", cfg.TargetOS+"_"+arch, name+exe)
	}
	cfg.SyzFuzzerBin = config.EnvReplace(targetBin("syz-fuzzer", cfg.TargetVMArch))
	cfg.SyzSchedBin = config.EnvReplace(targetBin("syz-scheduler", cfg.TargetVMArch))
	cfg.SyzExecprogBin = config.EnvReplace(targetBin("syz-execprog", cfg.TargetVMArch))
	cfg.SyzExecutorBin = config.EnvReplace(targetBin("syz-executor", cfg.TargetArch))

	// Disable sched_setaffinity, we can't fuzzing sched_setaffinity
	cfg.Disable_Syscalls = append(cfg.Disable_Syscalls, "sched_setaffinity")

	// TODO: Is there any better way?
	cfg.Syzkaller = config.EnvReplace(cfg.Syzkaller)
	cfg.Mempair = config.EnvReplace(cfg.Mempair)
	cfg.Mapping = config.EnvReplace(cfg.Mapping)
	cfg.CallGraph = config.EnvReplace(cfg.CallGraph)
	cfg.Distance = config.EnvReplace(cfg.Distance)
	cfg.Disable_Mempair = config.EnvReplace(cfg.Disable_Mempair)

	cfg.Workdir = config.EnvReplace(cfg.Workdir)
	cfg.Vmlinux = config.EnvReplace(cfg.Vmlinux)
	cfg.Kernel_Src = config.EnvReplace(cfg.Kernel_Src)
	cfg.Image = config.EnvReplace(cfg.Image)
	cfg.SSHKey = config.EnvReplace(cfg.SSHKey)
	if !osutil.IsExist(cfg.SyzFuzzerBin) {
		return nil, fmt.Errorf("bad config syzkaller param: can't find %v", cfg.SyzFuzzerBin)
	}
	if !osutil.IsExist(cfg.SyzExecprogBin) {
		return nil, fmt.Errorf("bad config syzkaller param: can't find %v", cfg.SyzExecprogBin)
	}
	if !osutil.IsExist(cfg.SyzExecutorBin) {
		return nil, fmt.Errorf("bad config syzkaller param: can't find %v", cfg.SyzExecutorBin)
	}
	if cfg.HTTP == "" {
		return nil, fmt.Errorf("config param http is empty")
	}
	if cfg.Workdir == "" {
		return nil, fmt.Errorf("config param workdir is empty")
	}
	if cfg.Type == "" {
		return nil, fmt.Errorf("config param type is empty")
	}
	if cfg.Procs < 1 || cfg.Procs > 32 {
		return nil, fmt.Errorf("bad config param procs: '%v', want [1, 32]", cfg.Procs)
	}
	switch cfg.Sandbox {
	case "none", "setuid", "namespace":
	default:
		return nil, fmt.Errorf("config param sandbox must contain one of none/setuid/namespace")
	}
	if cfg.SSHKey != "" {
		info, err := os.Stat(cfg.SSHKey)
		if err != nil {
			return nil, err
		}
		if info.Mode()&0077 != 0 {
			return nil, fmt.Errorf("sshkey %v is unprotected, ssh will reject it, do chmod 0600 on it", cfg.SSHKey)
		}
	}

	cfg.Workdir = osutil.Abs(cfg.Workdir)
	cfg.Vmlinux = osutil.Abs(cfg.Vmlinux)
	cfg.Syzkaller = osutil.Abs(cfg.Syzkaller)
	if cfg.Kernel_Src == "" {
		cfg.Kernel_Src = filepath.Dir(cfg.Vmlinux) // assume in-tree build by default
	}

	if err := parseSuppressions(cfg); err != nil {
		return nil, err
	}

	if cfg.Hub_Client != "" && (cfg.Name == "" || cfg.Hub_Addr == "" || cfg.Hub_Key == "") {
		return nil, fmt.Errorf("hub_client is set, but name/hub_addr/hub_key is empty")
	}
	if cfg.Dashboard_Client != "" && (cfg.Name == "" ||
		cfg.Dashboard_Addr == "" ||
		cfg.Dashboard_Key == "") {
		return nil, fmt.Errorf("dashboard_client is set, but name/dashboard_addr/dashboard_key is empty")
	}

	return cfg, nil
}

func SplitTarget(target string) (string, string, string, error) {
	if target == "" {
		return "", "", "", fmt.Errorf("target is empty")
	}
	targetParts := strings.Split(target, "/")
	if len(targetParts) != 2 && len(targetParts) != 3 {
		return "", "", "", fmt.Errorf("bad config param target")
	}
	os := targetParts[0]
	vmarch := targetParts[1]
	arch := targetParts[1]
	if len(targetParts) == 3 {
		arch = targetParts[2]
	}
	return os, vmarch, arch, nil
}

func ParseEnabledSyscalls(target *prog.Target, enabled, disabled []string) (map[int]bool, error) {
	syscalls := make(map[int]bool)
	if len(enabled) != 0 {
		for _, c := range enabled {
			n := 0
			for _, call := range target.Syscalls {
				if matchSyscall(call.Name, c) {
					syscalls[call.ID] = true
					n++
				}
			}
			if n == 0 {
				return nil, fmt.Errorf("unknown enabled syscall: %v", c)
			}
		}
	} else {
		for _, call := range target.Syscalls {
			syscalls[call.ID] = true
		}
	}
	for _, c := range disabled {
		n := 0
		for _, call := range target.Syscalls {
			if matchSyscall(call.Name, c) {
				delete(syscalls, call.ID)
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("unknown disabled syscall: %v", c)
		}
	}
	if len(syscalls) == 0 {
		return nil, fmt.Errorf("all syscalls are disabled by disable_syscalls in config")
	}
	return syscalls, nil
}

func matchSyscall(name, pattern string) bool {
	if pattern == name || strings.HasPrefix(name, pattern+"$") {
		return true
	}
	if len(pattern) > 1 && pattern[len(pattern)-1] == '*' &&
		strings.HasPrefix(name, pattern[:len(pattern)-1]) {
		return true
	}
	return false
}

func parseSuppressions(cfg *Config) error {
	// Add some builtin suppressions.
	// TODO(dvyukov): this should be moved to pkg/report.
	supp := append(cfg.Suppressions, []string{
		"panic: failed to start executor binary",
		"panic: executor failed: pthread_create failed",
		"panic: failed to create temp dir",
		"fatal error: runtime: out of memory",
		"fatal error: runtime: cannot allocate memory",
		"fatal error: unexpected signal during runtime execution", // presubmably OOM turned into SIGBUS
		"signal SIGBUS: bus error",                                // presubmably OOM turned into SIGBUS
		// TODO(dvyukov): these should be moved sys/targets as they are really linux-specific.
		"Out of memory: Kill process .* \\(syz-fuzzer\\)",
		"Out of memory: Kill process .* \\(sshd\\)",
		"Killed process .* \\(syz-fuzzer\\)",
		"Killed process .* \\(sshd\\)",
		"lowmemorykiller: Killing 'syz-fuzzer'",
		"lowmemorykiller: Killing 'sshd'",
		"INIT: PANIC: segmentation violation!",
	}...)
	for _, s := range supp {
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("failed to compile suppression '%v': %v", s, err)
		}
		cfg.ParsedSuppressions = append(cfg.ParsedSuppressions, re)
	}
	for _, ignore := range cfg.Ignores {
		re, err := regexp.Compile(ignore)
		if err != nil {
			return fmt.Errorf("failed to compile ignore '%v': %v", ignore, err)
		}
		cfg.ParsedIgnores = append(cfg.ParsedIgnores, re)
	}
	return nil
}

func CreateVMEnv(cfg *Config, debug, sched bool) *vm.Env {
	return &vm.Env{
		Name:    cfg.Name,
		OS:      cfg.TargetOS,
		Arch:    cfg.TargetVMArch,
		Workdir: cfg.Workdir,
		Image:   cfg.Image,
		SSHKey:  cfg.SSHKey,
		SSHUser: cfg.SSH_User,
		Debug:   debug,
		Config:  cfg.VM,
		Sched:   sched,
	}
}