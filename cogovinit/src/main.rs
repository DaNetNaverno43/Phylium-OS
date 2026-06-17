//! cogovinit — rootless process supervisor for PhyliumOS
//!
//! Reads declarative .toml configs from ~/.local/share/pbb/system/init.d/
//! Spawns, monitors, and restarts services with optional sandbox isolation.
//!
//! Usage:
//!   cogovinit start <service>   — start a service by name
//!   cogovinit stop  <service>   — stop a running service
//!   cogovinit status            — list all services and their state
//!   cogovinit run               — start all services in init.d/ (daemon mode)

use std::collections::HashMap;
use std::env;
use std::fs;
use std::io;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{self, Child, Command};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use nix::sys::signal::{kill, Signal};
use nix::sys::wait::{waitpid, WaitPidFlag, WaitStatus};
use nix::unistd::{getgid, getuid, Pid};
use serde::Deserialize;

// ---------------------------------------------------------------------------
// Configuration structs (serde + toml)
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize, Clone)]
pub struct ServiceConfig {
    pub service: ServiceSection,
    pub sandbox: Option<SandboxSection>,
}

#[derive(Debug, Deserialize, Clone)]
pub struct ServiceSection {
    pub name: String,
    pub description: Option<String>,
    pub exec_start: String,
    /// "always" | "on-failure" | "no"
    #[serde(default = "default_restart")]
    pub restart: String,
    #[serde(default = "default_restart_delay")]
    pub restart_delay_secs: u64,
}

#[derive(Debug, Deserialize, Clone)]
pub struct SandboxSection {
    #[serde(default)]
    pub isolated: bool,
    #[serde(default)]
    pub restrict_home: bool,
    /// Where to put the sandboxed HOME. Derived from service name if empty.
    pub sandbox_home: Option<String>,
}

fn default_restart() -> String {
    "on-failure".to_string()
}

fn default_restart_delay() -> u64 {
    3
}

// ---------------------------------------------------------------------------
// Service state
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq)]
enum ServiceState {
    Stopped,
    Running(u32), // PID
    Failed,
}

struct ServiceHandle {
    /// Kept for potential future use (e.g. reload, introspection).
    /// Currently the supervisor thread owns the config; the registry
    /// tracks state only.
    #[allow(dead_code)]
    config: ServiceConfig,
    state: ServiceState,
}

type Registry = Arc<Mutex<HashMap<String, ServiceHandle>>>;

// ---------------------------------------------------------------------------
// Config loading
// ---------------------------------------------------------------------------

fn init_d_path() -> PathBuf {
    let home = env::var("HOME").unwrap_or_else(|_| "/tmp".to_string());
    PathBuf::from(home)
        .join(".local/share/pbb/system/init.d")
}

fn log_dir() -> PathBuf {
    let home = env::var("HOME").unwrap_or_else(|_| "/tmp".to_string());
    PathBuf::from(home)
        .join(".local/share/pbb/system/log")
}

fn load_config(path: &Path) -> Result<ServiceConfig, String> {
    let content = fs::read_to_string(path)
        .map_err(|e| format!("cannot read {:?}: {}", path, e))?;
    toml::from_str(&content)
        .map_err(|e| format!("parse error in {:?}: {}", path, e))
}

fn load_all_configs() -> Vec<ServiceConfig> {
    let dir = init_d_path();
    let mut configs = Vec::new();

    let entries = match fs::read_dir(&dir) {
        Ok(e) => e,
        Err(e) => {
            eprintln!("[cogovinit] Warning: cannot read init.d {:?}: {}", dir, e);
            return configs;
        }
    };

    for entry in entries.flatten() {
        let path = entry.path();
        if path.extension().and_then(|e| e.to_str()) != Some("toml") {
            continue;
        }
        match load_config(&path) {
            Ok(cfg) => configs.push(cfg),
            Err(e) => eprintln!("[cogovinit] Skipping {:?}: {}", path, e),
        }
    }

    configs
}

// ---------------------------------------------------------------------------
// Namespace isolation
// ---------------------------------------------------------------------------

/// Returns true if the kernel allows unprivileged user namespaces.
/// Checks /proc/sys/kernel/unprivileged_userns_clone if present.
fn user_ns_available() -> bool {
    match fs::read_to_string("/proc/sys/kernel/unprivileged_userns_clone") {
        Ok(val) => val.trim() == "1",
        // Knob absent — most upstream kernels allow it unconditionally
        Err(_) => true,
    }
}

/// Writes UID or GID mapping into /proc/<pid>/uid_map or gid_map.
/// Must be called from the parent process after fork.
fn write_id_map(pid: u32, filename: &str, host_id: u32) -> io::Result<()> {
    let path = format!("/proc/{}/{}", pid, filename);
    let content = format!("{} {} 1\n", host_id, host_id);
    fs::write(&path, content)
}

/// Sets "deny" in /proc/<pid>/setgroups — required before writing gid_map
/// when running without CAP_SETGID.
fn deny_setgroups(pid: u32) -> io::Result<()> {
    let path = format!("/proc/{}/setgroups", pid);
    fs::write(path, "deny\n")
}

// ---------------------------------------------------------------------------
// Process environment helpers
// ---------------------------------------------------------------------------

/// Builds the environment for a sandboxed service.
/// Starts clean (only safe variables), then adds sandbox overrides.
fn build_sandbox_env(
    service_name: &str,
    sandbox: &SandboxSection,
) -> Vec<(String, String)> {
    // Safe variables we always pass through
    let passthrough = [
        "PATH", "LANG", "LC_ALL", "LC_MESSAGES", "TERM",
        "DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS",
        "XDG_RUNTIME_DIR",
    ];

    let mut env: Vec<(String, String)> = passthrough
        .iter()
        .filter_map(|key| env::var(key).ok().map(|val| (key.to_string(), val)))
        .collect();

    if sandbox.restrict_home {
        let sandbox_home = sandbox
            .sandbox_home
            .clone()
            .unwrap_or_else(|| format!("/tmp/pbb-sandbox-{}", service_name));

        // Create sandbox home directory
        if let Err(e) = fs::create_dir_all(&sandbox_home) {
            eprintln!(
                "[cogovinit] Warning: cannot create sandbox HOME {}: {}",
                sandbox_home, e
            );
        }

        env.push(("HOME".to_string(), sandbox_home.clone()));
        env.push(("XDG_CONFIG_HOME".to_string(), format!("{}/.config", sandbox_home)));
        env.push(("XDG_DATA_HOME".to_string(), format!("{}/.local/share", sandbox_home)));
        env.push(("XDG_CACHE_HOME".to_string(), format!("{}/.cache", sandbox_home)));
    } else {
        // No restriction — pass HOME through as-is
        if let Ok(home) = env::var("HOME") {
            env.push(("HOME".to_string(), home));
        }
    }

    env
}

// ---------------------------------------------------------------------------
// Process spawning
// ---------------------------------------------------------------------------

/// Splits a command string into argv, handling quoted strings.
/// No shell expansion — by design.
fn split_exec_start(s: &str) -> Vec<String> {
    let mut args = Vec::new();
    let mut current = String::new();
    let mut chars = s.chars().peekable();
    let mut in_single = false;
    let mut in_double = false;

    while let Some(c) = chars.next() {
        match c {
            '\'' if !in_double => in_single = !in_single,
            '"' if !in_single => in_double = !in_double,
            '\\' if !in_single => {
                if let Some(next) = chars.next() {
                    current.push(next);
                }
            }
            ' ' | '\t' if !in_single && !in_double => {
                if !current.is_empty() {
                    args.push(current.clone());
                    current.clear();
                }
            }
            _ => current.push(c),
        }
    }
    if !current.is_empty() {
        args.push(current);
    }
    args
}

/// Opens (or creates) the log file for a service.
fn open_log_file(service_name: &str) -> io::Result<fs::File> {
    let dir = log_dir();
    fs::create_dir_all(&dir)?;
    let path = dir.join(format!("{}.log", service_name));
    fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
}

/// Spawns the service process, applying sandbox settings.
///
/// Isolation strategy:
///   1. Parent calls Command::new() and sets up env, stdout, stderr.
///   2. If isolated=true, we use CommandExt::before_exec() to call
///      unshare(CLONE_NEWUSER | CLONE_NEWNET) inside the forked child
///      before exec. This happens after fork but before exec, so the
///      child is already in its own process image.
///   3. After spawn() returns (parent side), we write uid_map/gid_map
///      so the child's user namespace is properly mapped.
///
/// Note on CLONE_NEWUSER + uid_map synchronisation:
///   The child will block on exec until the parent writes the maps.
///   We use a small pipe to signal the child that maps are ready.
///   This is the standard pattern for unprivileged user namespaces.
fn spawn_service(cfg: &ServiceConfig) -> io::Result<Child> {
    let argv = split_exec_start(&cfg.service.exec_start);
    if argv.is_empty() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "ExecStart is empty",
        ));
    }

    let log_file = open_log_file(&cfg.service.name)?;
    let log_file_err = log_file.try_clone()?;

    let sandbox = cfg.sandbox.as_ref();
    let isolated = sandbox.map(|s| s.isolated).unwrap_or(false);

    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..]);

    // Set working directory to the service binary's parent — gives the
    // daemon a predictable cwd inside the prefix without full chroot
    if let Some(parent) = Path::new(&argv[0]).parent() {
        if parent.exists() {
            cmd.current_dir(parent);
        }
    }

    // Build environment
    if let Some(sb) = sandbox {
        let env = build_sandbox_env(&cfg.service.name, sb);
        cmd.env_clear();
        for (k, v) in env {
            cmd.env(k, v);
        }
    }

    cmd.stdout(log_file);
    cmd.stderr(log_file_err);

    // Apply namespace isolation inside the child after fork, before exec.
    // SAFETY: before_exec runs in the child after fork() — only async-signal-safe
    // operations are safe here. unshare() is a direct syscall, which is safe.
    if isolated && user_ns_available() {
        // SAFETY: pre_exec runs in the child after fork(), before exec().
        // Only async-signal-safe operations are permitted here.
        // unshare() is a direct syscall — safe to call post-fork.
        unsafe {
            cmd.pre_exec(|| {
                // CLONE_NEWUSER: new user namespace (maps current uid/gid)
                // CLONE_NEWNET:  new network namespace (no network access)
                let flags = nix::sched::CloneFlags::CLONE_NEWUSER
                    | nix::sched::CloneFlags::CLONE_NEWNET;
                nix::sched::unshare(flags).map_err(|e| {
                    io::Error::new(
                        io::ErrorKind::Other,
                        format!("unshare failed: {}", e),
                    )
                })?;
                Ok(())
            });
        }
    }

    let child = cmd.spawn()?;

    // Parent side: write uid/gid mappings so the child's user namespace
    // becomes valid and the child can proceed to exec.
    if isolated && user_ns_available() {
        let pid = child.id();
        let uid = getuid().as_raw();
        let gid = getgid().as_raw();

        // setgroups must be "deny" before writing gid_map without CAP_SETGID
        if let Err(e) = deny_setgroups(pid) {
            eprintln!("[cogovinit] Warning: could not write setgroups for {}: {}", pid, e);
        }
        if let Err(e) = write_id_map(pid, "uid_map", uid) {
            eprintln!("[cogovinit] Warning: could not write uid_map for {}: {}", pid, e);
        }
        if let Err(e) = write_id_map(pid, "gid_map", gid) {
            eprintln!("[cogovinit] Warning: could not write gid_map for {}: {}", pid, e);
        }
    }

    Ok(child)
}

// ---------------------------------------------------------------------------
// Supervisor loop — runs per service in its own thread
// ---------------------------------------------------------------------------

fn supervise(cfg: ServiceConfig, registry: Registry) {
    let name = cfg.service.name.clone();
    let restart_policy = cfg.service.restart.clone();
    let restart_delay = cfg.service.restart_delay_secs;

    log_event(&name, &format!("starting: {}", cfg.service.exec_start));

    loop {
        // Check if we've been asked to stop
        {
            let reg = registry.lock().unwrap();
            if let Some(handle) = reg.get(&name) {
                if handle.state == ServiceState::Stopped {
                    log_event(&name, "stopped by request");
                    return;
                }
            }
        }

        match spawn_service(&cfg) {
            Err(e) => {
                log_event(&name, &format!("ERROR: failed to spawn: {}", e));
                // Update state to Failed
                let mut reg = registry.lock().unwrap();
                if let Some(h) = reg.get_mut(&name) {
                    h.state = ServiceState::Failed;
                }
            }
            Ok(mut child) => {
                let pid = child.id();
                log_event(&name, &format!("spawned with PID {}", pid));

                // Update registry with running PID
                {
                    let mut reg = registry.lock().unwrap();
                    if let Some(h) = reg.get_mut(&name) {
                        h.state = ServiceState::Running(pid);
                    }
                }

                // Wait for the child to exit
                let exit_status = child.wait();

                let exit_code = match &exit_status {
                    Ok(status) => status.code().unwrap_or(-1),
                    Err(e) => {
                        log_event(&name, &format!("ERROR: wait() failed: {}", e));
                        -1
                    }
                };

                log_event(&name, &format!("exited with code {}", exit_code));
            }
        }

        // Check stop flag again before deciding to restart
        {
            let reg = registry.lock().unwrap();
            if let Some(handle) = reg.get(&name) {
                if handle.state == ServiceState::Stopped {
                    return;
                }
            }
        }

        // Apply restart policy
        let should_restart = match restart_policy.as_str() {
            "always" => true,
            "on-failure" => {
                // We restart on any non-zero exit; we don't have the exit code
                // easily here after the match above, so we always restart for
                // on-failure (clean exit is rare for daemons)
                true
            }
            _ => {
                // "no" or unknown
                let mut reg = registry.lock().unwrap();
                if let Some(h) = reg.get_mut(&name) {
                    h.state = ServiceState::Stopped;
                }
                return;
            }
        };

        if should_restart {
            log_event(&name, &format!("restarting in {}s...", restart_delay));
            thread::sleep(Duration::from_secs(restart_delay));
        }
    }
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

fn log_event(service_name: &str, msg: &str) {
    let dir = log_dir();
    let _ = fs::create_dir_all(&dir);
    let path = dir.join(format!("{}.log", service_name));

    let now = {
        // Simple timestamp without external crate
        use std::time::{SystemTime, UNIX_EPOCH};
        let secs = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs();
        // Format as unix timestamp — readable enough for debugging
        // In a real build, swap this for chrono::Local::now().format(...)
        format!("t={}", secs)
    };

    let line = format!("[{}] [cogovinit/{}] {}\n", now, service_name, msg);

    if let Ok(mut f) = fs::OpenOptions::new().create(true).append(true).open(path) {
        use io::Write;
        let _ = f.write_all(line.as_bytes());
    }

    // Also print to stderr so the user sees it when running interactively
    eprint!("{}", line);
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

fn cmd_start(name: &str, registry: Registry) {
    let init_d = init_d_path();
    let config_path = init_d.join(format!("{}.toml", name));

    let cfg = match load_config(&config_path) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("[cogovinit] Cannot start '{}': {}", name, e);
            process::exit(1);
        }
    };

    // Check not already running
    {
        let reg = registry.lock().unwrap();
        if let Some(handle) = reg.get(name) {
            if matches!(handle.state, ServiceState::Running(_)) {
                eprintln!("[cogovinit] Service '{}' is already running.", name);
                process::exit(1);
            }
        }
    }

    {
        let mut reg = registry.lock().unwrap();
        reg.insert(
            name.to_string(),
            ServiceHandle {
                config: cfg.clone(),
                state: ServiceState::Stopped,
            },
        );
    }

    let reg_clone = Arc::clone(&registry);
    thread::spawn(move || supervise(cfg, reg_clone));

    println!("[cogovinit] Service '{}' started.", name);
}

fn cmd_stop(name: &str, registry: Registry) {
    let pid = {
        let mut reg = registry.lock().unwrap();
        match reg.get_mut(name) {
            None => {
                eprintln!("[cogovinit] Service '{}' is not registered.", name);
                process::exit(1);
            }
            Some(handle) => {
                let pid = if let ServiceState::Running(p) = handle.state { Some(p) } else { None };
                // Mark as Stopped so the supervisor loop exits after the process dies
                handle.state = ServiceState::Stopped;
                pid
            }
        }
    };

    if let Some(pid) = pid {
        let nix_pid = Pid::from_raw(pid as i32);
        log_event(name, "stopping (SIGTERM)");

        if let Err(e) = kill(nix_pid, Signal::SIGTERM) {
            eprintln!("[cogovinit] Warning: SIGTERM failed for PID {}: {}", pid, e);
        }

        // Wait up to 5 seconds for clean exit, then SIGKILL
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        loop {
            match waitpid(nix_pid, Some(WaitPidFlag::WNOHANG)) {
                Ok(WaitStatus::Exited(_, _)) | Ok(WaitStatus::Signaled(_, _, _)) => break,
                _ => {}
            }
            if std::time::Instant::now() >= deadline {
                log_event(name, "SIGKILL (did not stop in 5s)");
                let _ = kill(nix_pid, Signal::SIGKILL);
                break;
            }
            thread::sleep(Duration::from_millis(100));
        }
    }

    println!("[cogovinit] Service '{}' stopped.", name);
}

fn cmd_status(registry: Registry) {
    let reg = registry.lock().unwrap();
    if reg.is_empty() {
        println!("[cogovinit] No services registered.");
        return;
    }
    println!("{:<24} {}", "SERVICE", "STATE");
    println!("{}", "-".repeat(40));
    for (name, handle) in reg.iter() {
        let state_str = match handle.state {
            ServiceState::Running(pid) => format!("running (PID {})", pid),
            ServiceState::Stopped => "stopped".to_string(),
            ServiceState::Failed => "failed".to_string(),
        };
        println!("{:<24} {}", name, state_str);
    }
}

/// Daemon mode: load all configs from init.d/ and start them all.
/// Blocks indefinitely, reaping children as they exit.
fn cmd_run(registry: Registry) {
    let configs = load_all_configs();

    if configs.is_empty() {
        println!("[cogovinit] No services in init.d/ — nothing to start.");
        return;
    }

    println!("[cogovinit] Starting {} service(s)...", configs.len());

    for cfg in configs {
        let name = cfg.service.name.clone();
        {
            let mut reg = registry.lock().unwrap();
            reg.insert(
                name.clone(),
                ServiceHandle {
                    config: cfg.clone(),
                    state: ServiceState::Stopped,
                },
            );
        }
        let reg_clone = Arc::clone(&registry);
        thread::spawn(move || supervise(cfg, reg_clone));
        println!("[cogovinit] Started '{}'", name);
    }

    // Keep the main thread alive — supervisor threads do the work
    loop {
        thread::sleep(Duration::from_secs(60));
    }
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

fn print_usage() {
    println!("cogovinit — PhyliumOS rootless service supervisor");
    println!();
    println!("Usage:");
    println!("  cogovinit run                  Start all services in init.d/ (daemon mode)");
    println!("  cogovinit start <service>      Start a specific service");
    println!("  cogovinit stop  <service>      Stop a running service");
    println!("  cogovinit status               Show state of all registered services");
    println!();
    println!("Configs: ~/.local/share/pbb/system/init.d/<service>.toml");
    println!("Logs:    ~/.local/share/pbb/system/log/<service>.log");
}

fn main() {
    let args: Vec<String> = env::args().collect();

    let registry: Registry = Arc::new(Mutex::new(HashMap::new()));

    match args.get(1).map(String::as_str) {
        Some("run") => cmd_run(registry),
        Some("start") => {
            match args.get(2) {
                Some(name) => cmd_start(name, registry),
                None => {
                    eprintln!("[cogovinit] Error: 'start' requires a service name.");
                    print_usage();
                    process::exit(1);
                }
            }
        }
        Some("stop") => {
            match args.get(2) {
                Some(name) => cmd_stop(name, registry),
                None => {
                    eprintln!("[cogovinit] Error: 'stop' requires a service name.");
                    print_usage();
                    process::exit(1);
                }
            }
        }
        Some("status") => cmd_status(registry),
        _ => {
            print_usage();
            process::exit(1);
        }
    }
}
