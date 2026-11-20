use std::ffi::CString;
use std::fs::{self, File, OpenOptions};
use std::io::{self, BufRead, BufReader, Read, Write};
use std::os::fd::{AsRawFd, OwnedFd};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicI32, AtomicU32, Ordering};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use nix::errno::Errno;
use nix::mount::{mount, umount2, MntFlags, MsFlags};
use nix::sched::{unshare, CloneFlags};
use nix::sys::signal::{kill, Signal};
use nix::sys::wait::{waitpid, WaitPidFlag, WaitStatus};
use nix::unistd::{chdir, execvp, fork, pipe, pivot_root, ForkResult, Pid};
use seccompiler::{apply_filter, BpfProgram, SeccompAction, SeccompFilter, TargetArch};
use serde::Serialize;

use crate::net;

const CGROUP_ROOT: &str = "/sys/fs/cgroup";
const DEFAULT_STATE_DIR: &str = "/run/minibox";
const DEFAULT_IMAGE_DIR: &str = "/var/lib/minibox/images";
const SETUP_FAILED: i32 = 126;
const TIMED_OUT: i32 = 124;
const DEV_NODES: [&str; 5] = ["null", "zero", "random", "urandom", "tty"];
const DEFAULT_PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";

/// Older system call numbers that only exist on x86_64. musl and some older binaries
/// (Alpine's busybox among them) use them for stat, open, and friends. On other
/// architectures this list is empty, and that architecture needs its own complete list.
#[cfg(target_arch = "x86_64")]
const LEGACY_SYSCALLS: &[libc::c_long] = &[
    libc::SYS_stat, libc::SYS_lstat, libc::SYS_open, libc::SYS_access, libc::SYS_pipe,
    libc::SYS_readlink, libc::SYS_getdents, libc::SYS_mkdir, libc::SYS_rmdir,
    libc::SYS_unlink, libc::SYS_rename, libc::SYS_creat, libc::SYS_link, libc::SYS_symlink,
    libc::SYS_chmod, libc::SYS_chown, libc::SYS_lchown, libc::SYS_mknod, libc::SYS_utime,
    libc::SYS_utimes, libc::SYS_dup2, libc::SYS_select, libc::SYS_poll, libc::SYS_time,
    libc::SYS_alarm, libc::SYS_pause, libc::SYS_getpgrp, libc::SYS_arch_prctl,
    libc::SYS_fork, libc::SYS_vfork, libc::SYS_epoll_create, libc::SYS_eventfd,
    libc::SYS_signalfd, libc::SYS_inotify_init, libc::SYS_epoll_wait,
];
#[cfg(not(target_arch = "x86_64"))]
const LEGACY_SYSCALLS: &[libc::c_long] = &[];

/// Root inside a job is this unprivileged host uid (and gid). The job's 65536 ids map to
/// host ids 100000 to 165535, so a job's "root" has no power over the host.
pub const BASE_ID: u32 = 100_000;
pub const ID_COUNT: u32 = 65_536;

/// Syscalls a job may use. Everything else fails with EPERM. The list covers what ordinary
/// programs need (files, memory, processes, sockets, timers) and leaves out anything that
/// changes the host or reaches other processes: mount, pivot_root, unshare, setns, ptrace,
/// bpf, module loading, reboot, and so on.
const ALLOWED_SYSCALLS: &[libc::c_long] = &[
    // files
    libc::SYS_read, libc::SYS_write, libc::SYS_readv, libc::SYS_writev,
    libc::SYS_pread64, libc::SYS_pwrite64, libc::SYS_preadv, libc::SYS_pwritev,
    libc::SYS_preadv2, libc::SYS_pwritev2, libc::SYS_openat, libc::SYS_openat2,
    libc::SYS_close, libc::SYS_close_range, libc::SYS_lseek, libc::SYS_fstat,
    libc::SYS_newfstatat, libc::SYS_statx, libc::SYS_statfs, libc::SYS_fstatfs,
    libc::SYS_faccessat, libc::SYS_faccessat2, libc::SYS_mkdirat, libc::SYS_mknodat,
    libc::SYS_unlinkat, libc::SYS_renameat, libc::SYS_renameat2, libc::SYS_linkat,
    libc::SYS_symlinkat, libc::SYS_readlinkat, libc::SYS_fchmod, libc::SYS_fchmodat,
    libc::SYS_fchownat, libc::SYS_fchown, libc::SYS_ftruncate, libc::SYS_fallocate,
    libc::SYS_fsync, libc::SYS_fdatasync, libc::SYS_sync_file_range, libc::SYS_syncfs,
    libc::SYS_sync, libc::SYS_getdents64, libc::SYS_getcwd, libc::SYS_chdir,
    libc::SYS_fchdir, libc::SYS_utimensat, libc::SYS_umask, libc::SYS_dup,
    libc::SYS_dup3, libc::SYS_pipe2, libc::SYS_fcntl, libc::SYS_flock, libc::SYS_ioctl,
    libc::SYS_fgetxattr, libc::SYS_getxattr, libc::SYS_lgetxattr, libc::SYS_listxattr,
    libc::SYS_flistxattr, libc::SYS_llistxattr, libc::SYS_readahead, libc::SYS_splice,
    libc::SYS_tee, libc::SYS_vmsplice, libc::SYS_copy_file_range, libc::SYS_sendfile,
    libc::SYS_inotify_init1, libc::SYS_inotify_add_watch, libc::SYS_inotify_rm_watch,
    // memory
    libc::SYS_mmap, libc::SYS_mprotect, libc::SYS_munmap, libc::SYS_mremap,
    libc::SYS_brk, libc::SYS_madvise, libc::SYS_mincore, libc::SYS_msync,
    libc::SYS_mlock, libc::SYS_munlock, libc::SYS_mlockall, libc::SYS_munlockall,
    libc::SYS_memfd_create,
    // signals
    libc::SYS_rt_sigaction, libc::SYS_rt_sigprocmask, libc::SYS_rt_sigreturn,
    libc::SYS_rt_sigpending, libc::SYS_rt_sigtimedwait, libc::SYS_rt_sigqueueinfo,
    libc::SYS_rt_sigsuspend, libc::SYS_sigaltstack, libc::SYS_kill, libc::SYS_tgkill,
    libc::SYS_tkill, libc::SYS_signalfd4,
    // processes and identity
    libc::SYS_clone, libc::SYS_clone3,
    libc::SYS_execve, libc::SYS_execveat, libc::SYS_exit, libc::SYS_exit_group,
    libc::SYS_wait4, libc::SYS_waitid, libc::SYS_getpid, libc::SYS_getppid,
    libc::SYS_gettid, libc::SYS_getpgid, libc::SYS_setpgid,
    libc::SYS_getsid, libc::SYS_setsid, libc::SYS_set_tid_address,
    libc::SYS_set_robust_list, libc::SYS_get_robust_list, libc::SYS_prctl,
    libc::SYS_rseq, libc::SYS_getuid, libc::SYS_geteuid,
    libc::SYS_getgid, libc::SYS_getegid, libc::SYS_setuid, libc::SYS_setgid,
    libc::SYS_setreuid, libc::SYS_setregid, libc::SYS_setresuid, libc::SYS_setresgid,
    libc::SYS_getresuid, libc::SYS_getresgid, libc::SYS_setfsuid, libc::SYS_setfsgid,
    libc::SYS_getgroups, libc::SYS_setgroups, libc::SYS_capget, libc::SYS_capset,
    libc::SYS_getpriority, libc::SYS_setpriority, libc::SYS_personality, libc::SYS_uname,
    libc::SYS_sched_yield, libc::SYS_sched_getaffinity, libc::SYS_sched_setaffinity,
    libc::SYS_sched_getparam, libc::SYS_sched_getscheduler, libc::SYS_sched_setscheduler,
    libc::SYS_sched_get_priority_max, libc::SYS_sched_get_priority_min,
    libc::SYS_getcpu, libc::SYS_futex, libc::SYS_restart_syscall,
    // time and limits
    libc::SYS_nanosleep, libc::SYS_clock_nanosleep, libc::SYS_clock_gettime,
    libc::SYS_clock_getres, libc::SYS_gettimeofday, libc::SYS_times,
    libc::SYS_timer_create, libc::SYS_timer_settime, libc::SYS_timer_gettime,
    libc::SYS_timer_delete, libc::SYS_timerfd_create, libc::SYS_timerfd_settime,
    libc::SYS_timerfd_gettime, libc::SYS_setitimer, libc::SYS_getitimer,
    libc::SYS_getrlimit, libc::SYS_setrlimit, libc::SYS_prlimit64, libc::SYS_getrusage,
    libc::SYS_sysinfo, libc::SYS_getrandom,
    // event loops
    libc::SYS_pselect6, libc::SYS_ppoll,
    libc::SYS_epoll_create1, libc::SYS_epoll_ctl,
    libc::SYS_epoll_pwait, libc::SYS_eventfd2,
    // sockets
    libc::SYS_socket, libc::SYS_socketpair, libc::SYS_connect, libc::SYS_bind,
    libc::SYS_listen, libc::SYS_accept, libc::SYS_accept4, libc::SYS_sendto,
    libc::SYS_recvfrom, libc::SYS_sendmsg, libc::SYS_recvmsg, libc::SYS_sendmmsg,
    libc::SYS_recvmmsg, libc::SYS_shutdown, libc::SYS_getsockname, libc::SYS_getpeername,
    libc::SYS_setsockopt, libc::SYS_getsockopt,
];

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub enum NetMode {
    /// New network namespace with only loopback. The default.
    #[default]
    Isolated,
    /// Share the host's network namespace.
    Host,
    /// New namespace on the minibox0 bridge, with NAT to the outside.
    Bridge,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Volume {
    pub host: PathBuf,
    pub guest: PathBuf,
    pub read_only: bool,
}

#[derive(Debug, Default, Clone)]
pub struct Config {
    pub mem_mb: Option<u64>,
    pub pids: Option<u64>,
    pub cpus: Option<f64>,
    pub timeout_secs: Option<u64>,
    pub rootfs: Option<PathBuf>,
    pub image: Option<String>,
    pub net: NetMode,
    pub volumes: Vec<Volume>,
    pub env: Vec<(String, String)>,
    pub workdir: Option<PathBuf>,
    pub cmd: Vec<String>,
}

/// Paths for one run's copy-on-write root filesystem.
#[derive(Debug, Clone)]
struct Overlay {
    lower: PathBuf,
    run_dir: PathBuf,
    upper: PathBuf,
    work: PathBuf,
    merged: PathBuf,
}

pub fn run_cli(args: Vec<String>) -> i32 {
    if args.first().map(String::as_str) == Some("image") {
        return image_cmd(&args[1..]);
    }
    match parse(&args) {
        Ok(cfg) => match run(&cfg) {
            Ok(code) => code,
            Err(e) => {
                eprintln!("minibox: {e}");
                1
            }
        },
        Err(msg) => {
            eprintln!("minibox: {msg}");
            eprintln!(
                "usage: minibox run [--image NAME | --rootfs DIR] [--net host|isolated|bridge]\n\
                 \x20                  [--mem MB] [--cpus N] [--pids N] [--timeout SECS]\n\
                 \x20                  [-v HOST:GUEST[:ro]]... [-e K=V]... [-w DIR] -- CMD [ARGS...]\n\
                 \x20      minibox image import NAME TARBALL | minibox image pull NAME URL [--sha256 HEX] | minibox image ls"
            );
            2
        }
    }
}

fn parse(args: &[String]) -> Result<Config, String> {
    if args.first().map(String::as_str) != Some("run") {
        return Err("expected 'run' or 'image'".into());
    }
    let mut cfg = Config::default();
    let mut i = 1;
    while i < args.len() {
        let flag = args[i].as_str();
        match flag {
            "--" => {
                i += 1;
                break;
            }
            "--mem" => cfg.mem_mb = Some(parse_num(take(args, &mut i, flag)?, flag)?),
            "--pids" => cfg.pids = Some(parse_num(take(args, &mut i, flag)?, flag)?),
            "--timeout" => cfg.timeout_secs = Some(parse_num(take(args, &mut i, flag)?, flag)?),
            "--cpus" => {
                let v = take(args, &mut i, flag)?;
                let n: f64 = v
                    .parse()
                    .map_err(|_| format!("--cpus must be a number, got {v}"))?;
                if n <= 0.0 {
                    return Err("--cpus must be greater than 0".into());
                }
                cfg.cpus = Some(n);
            }
            "--rootfs" => cfg.rootfs = Some(PathBuf::from(take(args, &mut i, flag)?)),
            "--image" => cfg.image = Some(take(args, &mut i, flag)?.clone()),
            "--net" => {
                cfg.net = match take(args, &mut i, flag)?.as_str() {
                    "host" => NetMode::Host,
                    "isolated" => NetMode::Isolated,
                    "bridge" => NetMode::Bridge,
                    _ => return Err("--net takes host, isolated or bridge".into()),
                }
            }
            "-v" | "--volume" => cfg.volumes.push(parse_volume(take(args, &mut i, flag)?)?),
            "-e" | "--env" => cfg.env.push(parse_env(take(args, &mut i, flag)?)?),
            "-w" | "--workdir" => cfg.workdir = Some(PathBuf::from(take(args, &mut i, flag)?)),
            other => return Err(format!("unknown option {other}")),
        }
        i += 1;
    }
    cfg.cmd = args[i.min(args.len())..].to_vec();
    if cfg.cmd.is_empty() {
        return Err("no command given after --".into());
    }
    if cfg.image.is_some() && cfg.rootfs.is_some() {
        return Err("use either --image or --rootfs, not both".into());
    }
    if !cfg.volumes.is_empty() && cfg.rootfs.is_none() && cfg.image.is_none() {
        return Err("volumes (-v) need --image or --rootfs".into());
    }
    Ok(cfg)
}

/// Returns the value after the flag at `*i` and moves `*i` onto it.
fn take<'a>(args: &'a [String], i: &mut usize, flag: &str) -> Result<&'a String, String> {
    *i += 1;
    args.get(*i)
        .ok_or_else(|| format!("{flag} needs a value"))
}

fn parse_num(value: &str, flag: &str) -> Result<u64, String> {
    value
        .parse::<u64>()
        .map_err(|_| format!("{flag} must be a whole number, got {value}"))
}

fn parse_volume(spec: &str) -> Result<Volume, String> {
    let parts: Vec<&str> = spec.split(':').collect();
    let (host, guest, mode) = match parts.as_slice() {
        [h, g] => (*h, *g, "rw"),
        [h, g, m] => (*h, *g, *m),
        _ => return Err(format!("bad volume '{spec}', expected HOST:GUEST[:ro]")),
    };
    let read_only = match mode {
        "ro" => true,
        "rw" => false,
        _ => return Err(format!("volume mode must be ro or rw, got {mode}")),
    };
    if !host.starts_with('/') || !guest.starts_with('/') {
        return Err(format!("volume paths must be absolute: {spec}"));
    }
    Ok(Volume {
        host: PathBuf::from(host),
        guest: PathBuf::from(guest),
        read_only,
    })
}

fn parse_env(spec: &str) -> Result<(String, String), String> {
    match spec.split_once('=') {
        Some((k, v)) if !k.is_empty() => Ok((k.to_string(), v.to_string())),
        _ => Err(format!("bad env '{spec}', expected KEY=VALUE")),
    }
}

// ---- images -------------------------------------------------------------

pub(crate) fn image_dir() -> PathBuf {
    std::env::var_os("MINIBOX_IMAGE_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(DEFAULT_IMAGE_DIR))
}

fn image_cmd(args: &[String]) -> i32 {
    let result = match args.first().map(String::as_str) {
        Some("import") if args.len() == 3 => import_image(&args[1], Path::new(&args[2])),
        Some("pull-registry") if args.len() == 3 => crate::registry::pull(&args[1], &args[2]),
        Some("pull") if args.len() == 3 => pull_image(&args[1], &args[2], None),
        Some("pull") if args.len() == 5 && args[3] == "--sha256" => {
            pull_image(&args[1], &args[2], Some(&args[4]))
        }
        Some("ls") => list_images(),
        _ => Err("usage: minibox image import NAME TARBALL\n       \
                  minibox image pull NAME URL [--sha256 HEX]\n       \
                  minibox image ls"
            .into()),
    };
    match result {
        Ok(()) => 0,
        Err(e) => {
            eprintln!("minibox: {e}");
            1
        }
    }
}

/// Unpacks a root filesystem tarball into the image store under NAME. Every file is
/// owned by the job's root (BASE_ID), so jobs can write to the image's own layer.
fn import_image(name: &str, tarball: &Path) -> Result<(), String> {
    if name.is_empty() || name.contains('/') || name.starts_with('.') {
        return Err(format!("bad image name '{name}'"));
    }
    if !tarball.is_file() {
        return Err(format!("tarball {} not found", tarball.display()));
    }
    let dest = image_dir().join(name);
    if dest.exists() {
        return Err(format!("image '{name}' already exists"));
    }
    fs::create_dir_all(&dest).map_err(|e| format!("create {}: {e}", dest.display()))?;
    let owner = BASE_ID.to_string();
    let status = Command::new("tar")
        .arg("-xf")
        .arg(tarball)
        .arg("-C")
        .arg(&dest)
        .arg(format!("--owner={owner}"))
        .arg(format!("--group={owner}"))
        .status()
        .map_err(|e| format!("run tar: {e}"))?;
    if !status.success() {
        let _ = fs::remove_dir_all(&dest);
        return Err("tar failed to unpack the image".into());
    }
    // Make every file and directory in the image belong to the job's root.
    let chown = Command::new("chown")
        .args(["-R", &format!("{owner}:{owner}")])
        .arg(&dest)
        .status()
        .map_err(|e| format!("run chown: {e}"))?;
    if !chown.success() {
        let _ = fs::remove_dir_all(&dest);
        return Err("chown of the image failed".into());
    }
    println!("imported {name} from {}", tarball.display());
    Ok(())
}

/// Downloads an image tarball with curl, checks its SHA-256 if one was given, then imports it.
/// A download that doesn't match the expected checksum is never unpacked.
fn pull_image(name: &str, url: &str, expected_sha256: Option<&str>) -> Result<(), String> {
    let tmp = std::env::temp_dir().join(format!("minibox-pull-{}.tar", std::process::id()));
    let status = Command::new("curl")
        .args(["-fsSL", "--max-time", "600", "-o"])
        .arg(&tmp)
        .arg(url)
        .status()
        .map_err(|e| format!("run curl: {e}"))?;
    if !status.success() {
        let _ = fs::remove_file(&tmp);
        return Err(format!("download of {url} failed"));
    }

    let result = (|| -> Result<(), String> {
        let mut file = File::open(&tmp).map_err(|e| format!("open download: {e}"))?;
        let mut hasher = <sha2::Sha256 as sha2::Digest>::new();
        io::copy(&mut file, &mut hasher).map_err(|e| format!("hash download: {e}"))?;
        let digest = format!("{:x}", sha2::Digest::finalize(hasher));
        if let Some(want) = expected_sha256 {
            if !digest.eq_ignore_ascii_case(want.trim()) {
                return Err(format!(
                    "checksum mismatch: expected {want}, got {digest}; image not imported"
                ));
            }
        }
        import_image(name, &tmp)
    })();
    let _ = fs::remove_file(&tmp);
    result
}

fn list_images() -> Result<(), String> {
    let dir = image_dir();
    let entries = match fs::read_dir(&dir) {
        Ok(e) => e,
        Err(_) => {
            println!("no images in {}", dir.display());
            return Ok(());
        }
    };
    let mut names: Vec<String> = entries
        .flatten()
        .filter(|e| e.path().is_dir())
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    for n in names {
        println!("{n}");
    }
    Ok(())
}

// ---- run ----------------------------------------------------------------

/// Starts a job. The runner stays outside the job's namespaces and does the host-side
/// work: cgroups, veth pairs, state records, and collecting output.
fn run(cfg: &Config) -> Result<i32, String> {
    // Build every C string before fork so the child does no allocation.
    let argv: Vec<CString> = cfg
        .cmd
        .iter()
        .map(|s| CString::new(s.as_str()).map_err(|e| e.to_string()))
        .collect::<Result<_, _>>()?;

    if let Some(root) = &cfg.rootfs {
        if !root.is_dir() {
            return Err(format!("rootfs {} is not a directory", root.display()));
        }
    }
    for v in &cfg.volumes {
        if !v.host.exists() {
            return Err(format!("volume source {} does not exist", v.host.display()));
        }
    }
    if let Some(dir) = &cfg.workdir {
        if !dir.is_absolute() {
            return Err(format!("workdir must be absolute: {}", dir.display()));
        }
    }
    let id = std::process::id();

    // With --image, the job gets an overlay on top of the image so its writes stay put.
    let overlay = match &cfg.image {
        Some(name) => {
            let lower = image_dir().join(name);
            if !lower.is_dir() {
                return Err(format!(
                    "image '{name}' not found (import it with: minibox image import {name} TARBALL)"
                ));
            }
            let run_dir = state_dir().join(format!("run-{id}"));
            Some(Overlay {
                lower,
                upper: run_dir.join("upper"),
                work: run_dir.join("work"),
                merged: run_dir.join("merged"),
                run_dir,
            })
        }
        None => None,
    };
    let root = match &overlay {
        Some(o) => Some(o.merged.clone()),
        None => cfg.rootfs.clone(),
    };

    // Volumes are prepared before anything else, so a failed mount never leaves a sandbox behind.
    let (run_cfg, volume_mounts) = resolve_volumes(cfg, id)?;
    let _volumes = VolumeGuard(volume_mounts);

    if cfg.net == NetMode::Bridge {
        net::ensure_bridge()?;
    }
    gc_stale_cgroups();

    // Cleanup guards. Declared before anything is created so every error path cleans up.
    let _fs = FsGuard(overlay.clone());
    // Join the cgroup first. Children inherit it, so limits apply from the first instruction.
    let guard = CgroupGuard(make_cgroup(cfg)?);
    let mut state = StateFile {
        id,
        cmd: cfg.cmd.clone(),
        started_unix: now_unix(),
        cgroup: guard.0.as_ref().map(|p| p.display().to_string()),
        init_pid: None,
        ip: None,
    };
    state.save("running", None);

    // Pipes:
    //   down: runner -> stage1 (id mapping done, sandbox address)
    //   up:   stage1 -> runner (progress messages)
    //   out/err: sandbox output, copied to the terminal and the log by the runner
    // The overlay is mounted here, by the runner, in the host's mount namespace. The sandbox
    // inherits the mount when it unshares its own namespace. Overlayfs won't mount from
    // inside a user namespace for a host root that the namespace doesn't map. The job's
    // directories belong to the job's root, so the job can write to its own layer.
    if let Some(o) = &overlay {
        for dir in [&o.run_dir, &o.upper, &o.work, &o.merged] {
            fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
            chown_to_base(dir)?;
        }
        mount_overlay(o)?;
    }

    let (down_r, down_w) = pipe().map_err(|e| format!("pipe: {e}"))?;
    let (up_r, up_w) = pipe().map_err(|e| format!("pipe: {e}"))?;
    let (out_r, out_w) = pipe().map_err(|e| format!("pipe: {e}"))?;
    let (err_r, err_w) = pipe().map_err(|e| format!("pipe: {e}"))?;

    let runner_pid = std::process::id() as i32;
    // SAFETY: this program is single-threaded at this point, so fork is safe.
    match unsafe { fork() }.map_err(|e| format!("fork: {e}"))? {
        ForkResult::Child => {
            drop((down_w, up_r, out_r, err_r));
            stage1(StagePlan {
                cfg: &run_cfg,
                argv: &argv,
                root,
                down: down_r,
                up: up_w,
                stdout: out_w,
                stderr: err_w,
                runner_pid,
            })
        }
        ForkResult::Parent { child } => {
            drop((down_r, up_w, out_w, err_w));
            let log_path = state_dir().join(format!("{id}.log"));
            let mut out = OutputPump::new(out_r, err_r, &log_path)?;
            state.init_pid = Some(child.as_raw());

            if let Err(e) = handshake(cfg, child, up_r, down_w, &mut state) {
                // Stop the sandbox. Its init dies with it.
                let _ = kill(child, Signal::SIGKILL);
                let _ = waitpid(child, None);
                out.drain();
                return Err(e);
            }
            state.save("running", None);
            let (code, timed_out) =
                wait_child(child, cfg.timeout_secs.map(Duration::from_secs), &mut out)?;
            out.drain();
            state.save(if timed_out { "timed_out" } else { "exited" }, Some(code));
            drop(guard);
            Ok(code)
        }
    }
}

/// Runner side of the start-up handshake with stage1:
///   stage1 -> "net"      network namespace exists
///   runner -> address    veth pair is attached (or "-")
///   stage1 -> "userns"   user namespace exists
///   runner -> "mapped"   uid and gid maps are written
fn handshake(
    cfg: &Config,
    child: Pid,
    up: OwnedFd,
    down: OwnedFd,
    state: &mut StateFile,
) -> Result<(), String> {
    let mut up = BufReader::new(File::from(up));
    let mut down = File::from(down);

    expect_line(&mut up, "net")?;
    let addr = if cfg.net == NetMode::Bridge {
        let used = used_addresses(&state_dir());
        let addr = net::pick_address(&used).ok_or("no free addresses left on minibox0")?;
        net::attach(child.as_raw())?;
        state.ip = Some(addr.clone());
        addr
    } else {
        "-".to_string()
    };
    writeln!(down, "{addr}").map_err(|e| format!("send address to sandbox: {e}"))?;

    expect_line(&mut up, "userns")?;
    map_ids(child)?;
    writeln!(down, "mapped").map_err(|e| format!("tell sandbox ids are mapped: {e}"))?;
    Ok(())
}

/// Makes each volume usable by the job's root (BASE_ID). A folder that BASE_ID already owns
/// is bound as it is. Any other folder gets an idmapped mount, which shows its owner as
/// BASE_ID for that mount only. The folder's ownership on the host does not change.
/// Idmapped mounts need util-linux 2.39 or newer and Linux 5.12 or newer.
fn resolve_volumes(cfg: &Config, id: u32) -> Result<(Config, Vec<PathBuf>), String> {
    use std::os::unix::fs::MetadataExt;

    let mut out = cfg.clone();
    let mut mounted: Vec<PathBuf> = Vec::new();
    let result = (|| -> Result<(), String> {
        for (i, v) in cfg.volumes.iter().enumerate() {
            let owner = fs::metadata(&v.host)
                .map_err(|e| format!("stat {}: {e}", v.host.display()))?
                .uid();
            if owner == BASE_ID {
                continue;
            }
            let target = state_dir().join(format!("run-{id}")).join(format!("vol-{i}"));
            fs::create_dir_all(&target).map_err(|e| format!("create {}: {e}", target.display()))?;
            mounted.push(target.clone());
            let status = Command::new("mount")
                .arg("--bind")
                .arg("-o")
                .arg(format!("X-mount.idmap=b:{owner}:{BASE_ID}:1"))
                .arg(&v.host)
                .arg(&target)
                .output()
                .map_err(|e| format!("run mount: {e}"))?;
            if !status.status.success() {
                return Err(format!(
                    "cannot give the job access to {} (owned by uid {owner}). Idmapped mounts need util-linux 2.39+ and Linux 5.12+: {}",
                    v.host.display(),
                    String::from_utf8_lossy(&status.stderr).trim()
                ));
            }
            out.volumes[i].host = target;
        }
        Ok(())
    })();
    match result {
        Ok(()) => Ok((out, mounted)),
        Err(e) => {
            unmount_and_remove(&mounted);
            Err(e)
        }
    }
}

/// Unmounts volume mounts made for a run and removes their mount points.
struct VolumeGuard(Vec<PathBuf>);

impl Drop for VolumeGuard {
    fn drop(&mut self) {
        unmount_and_remove(&self.0);
    }
}

fn unmount_and_remove(paths: &[PathBuf]) {
    for p in paths.iter().rev() {
        let _ = umount2(p.as_path(), MntFlags::MNT_DETACH);
        let _ = fs::remove_dir(p);
    }
    // The run directory that held the mount points goes too, once it is empty.
    if let Some(parent) = paths.first().and_then(|p| p.parent()) {
        let _ = fs::remove_dir(parent);
    }
}

/// Hands a path to the job's root (BASE_ID) so the job can write to it.
fn chown_to_base(path: &Path) -> Result<(), String> {
    let status = Command::new("chown")
        .arg(format!("{BASE_ID}:{BASE_ID}"))
        .arg(path)
        .status()
        .map_err(|e| format!("chown {}: {e}", path.display()))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("chown {} failed", path.display()))
    }
}

fn expect_line(r: &mut impl BufRead, want: &str) -> Result<(), String> {
    let mut line = String::new();
    let n = r
        .read_line(&mut line)
        .map_err(|e| format!("sandbox setup: {e}"))?;
    if n == 0 {
        return Err("sandbox stopped during setup".into());
    }
    if line.trim() != want {
        return Err(format!("sandbox setup: expected '{want}', got '{}'", line.trim()));
    }
    Ok(())
}

/// Writes the uid and gid maps that make the job's root an unprivileged host user.
fn map_ids(pid: Pid) -> Result<(), String> {
    let p = pid.as_raw();
    fs::write(format!("/proc/{p}/setgroups"), "deny").map_err(|e| format!("setgroups: {e}"))?;
    let map = format!("0 {BASE_ID} {ID_COUNT}\n");
    fs::write(format!("/proc/{p}/uid_map"), &map).map_err(|e| format!("uid_map: {e}"))?;
    fs::write(format!("/proc/{p}/gid_map"), &map).map_err(|e| format!("gid_map: {e}"))?;
    Ok(())
}

/// Copies the sandbox's stdout and stderr pipes to the terminal and to the run's log file.
/// Single-threaded on purpose: spawning threads after unshare(CLONE_NEWPID) fails with EINVAL.
struct OutputPump {
    pipes: Vec<OutPipe>,
    log: File,
}

struct OutPipe {
    src: File,
    to_stderr: bool,
}

impl OutputPump {
    fn new(out_r: OwnedFd, err_r: OwnedFd, log_path: &Path) -> Result<Self, String> {
        let log = OpenOptions::new()
            .create(true)
            .append(true)
            .open(log_path)
            .map_err(|e| format!("open log {}: {e}", log_path.display()))?;
        Ok(OutputPump {
            pipes: vec![
                OutPipe {
                    src: File::from(out_r),
                    to_stderr: false,
                },
                OutPipe {
                    src: File::from(err_r),
                    to_stderr: true,
                },
            ],
            log,
        })
    }

    /// Waits up to `timeout_ms` for output, then copies whatever arrived.
    fn pump_once(&mut self, timeout_ms: i32) {
        if self.pipes.is_empty() {
            return;
        }
        let mut fds: Vec<libc::pollfd> = self
            .pipes
            .iter()
            .map(|p| libc::pollfd {
                fd: p.src.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            })
            .collect();
        // SAFETY: fds is a valid slice of pollfd structs for the duration of the call.
        let ready = unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, timeout_ms) };
        if ready <= 0 {
            return;
        }
        let mut buf = [0u8; 8192];
        let mut closed = Vec::new();
        for idx in 0..fds.len() {
            if fds[idx].revents == 0 {
                continue;
            }
            let to_stderr = self.pipes[idx].to_stderr;
            match self.pipes[idx].src.read(&mut buf) {
                Ok(0) | Err(_) => closed.push(idx),
                Ok(n) => self.emit(to_stderr, &buf[..n]),
            }
        }
        for idx in closed.into_iter().rev() {
            self.pipes.remove(idx);
        }
    }

    fn emit(&mut self, to_stderr: bool, data: &[u8]) {
        if to_stderr {
            let _ = io::stderr().write_all(data);
            let _ = io::stderr().flush();
        } else {
            let _ = io::stdout().write_all(data);
            let _ = io::stdout().flush();
        }
        let _ = self.log.write_all(data);
    }

    /// Copies output until both pipes reach end-of-file (every process in the sandbox has exited).
    fn drain(&mut self) {
        while !self.pipes.is_empty() {
            self.pump_once(1000);
        }
    }
}

/// Host-side pid of the sandbox's first process is what the runner tracks.
fn pid_or_none(pid: i32) -> Option<i32> {
    if pid > 0 {
        Some(pid)
    } else {
        None
    }
}

// ---- cgroup -------------------------------------------------------------

fn make_cgroup(cfg: &Config) -> Result<Option<PathBuf>, String> {
    if cfg.mem_mb.is_none() && cfg.pids.is_none() && cfg.cpus.is_none() {
        return Ok(None);
    }
    let root = PathBuf::from(CGROUP_ROOT);
    if !root.join("cgroup.controllers").exists() {
        return Err(format!(
            "cgroup v2 not found at {CGROUP_ROOT} (in WSL, check the kernel cgroup2 mount)"
        ));
    }
    // Ignore the error if the controllers are already enabled.
    let _ = fs::write(root.join("cgroup.subtree_control"), "+memory +pids +cpu");

    let dir = root.join(format!("minibox-{}", std::process::id()));
    fs::create_dir(&dir).map_err(|e| format!("create {}: {e}", dir.display()))?;

    let write = |name: &str, value: String| -> Result<(), String> {
        fs::write(dir.join(name), &value).map_err(|e| format!("write {name} = {value}: {e}"))
    };

    if let Some(mb) = cfg.mem_mb {
        write("memory.max", (mb * 1024 * 1024).to_string())?;
        let _ = fs::write(dir.join("memory.swap.max"), "0");
    }
    if let Some(n) = cfg.pids {
        write("pids.max", n.to_string())?;
    }
    if let Some(cpus) = cfg.cpus {
        // cpu.max is "quota period" in microseconds. 100ms period, quota scales with --cpus.
        let quota = ((cpus * 100_000.0) as u64).max(1000);
        write("cpu.max", format!("{quota} 100000"))?;
    }
    write("cgroup.procs", std::process::id().to_string())?;
    Ok(Some(dir))
}

/// Removes sandbox cgroups whose runner process is gone. A cgroup that still has
/// live processes in it cannot be removed, and that is fine.
fn gc_stale_cgroups() {
    let Ok(entries) = fs::read_dir(CGROUP_ROOT) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        let Some(pid) = name.strip_prefix("minibox-").and_then(|p| p.parse::<u32>().ok()) else {
            continue;
        };
        if !Path::new(&format!("/proc/{pid}")).exists() {
            let _ = fs::remove_dir(entry.path());
        }
    }
}

/// Moves the runner back to the root cgroup and removes the sandbox cgroup on drop,
/// so every error path cleans up.
struct CgroupGuard(Option<PathBuf>);

impl Drop for CgroupGuard {
    fn drop(&mut self) {
        if let Some(dir) = self.0.take() {
            let _ = fs::write(
                Path::new(CGROUP_ROOT).join("cgroup.procs"),
                std::process::id().to_string(),
            );
            let _ = fs::remove_dir(dir);
        }
    }
}

/// Removes a run's overlay scratch directories. The overlay mount lives in the sandbox's
/// own mount namespace, so it goes away when the sandbox does.
struct FsGuard(Option<Overlay>);

impl Drop for FsGuard {
    fn drop(&mut self) {
        if let Some(o) = self.0.take() {
            // The sandbox has exited, so nothing uses the overlay any more.
            let _ = umount2(o.merged.as_path(), MntFlags::MNT_DETACH);
            if let Err(e) = fs::remove_dir_all(&o.run_dir) {
                eprintln!("minibox: warning: remove {}: {e}", o.run_dir.display());
            }
        }
    }
}

// ---- state records ------------------------------------------------------

struct StateFile {
    id: u32,
    cmd: Vec<String>,
    started_unix: u64,
    cgroup: Option<String>,
    init_pid: Option<i32>,
    ip: Option<String>,
}

#[derive(Serialize)]
struct Record<'a> {
    id: u32,
    cmd: &'a [String],
    started_unix: u64,
    cgroup: Option<String>,
    init_pid: Option<i32>,
    ip: Option<String>,
    status: &'a str,
    exit_code: Option<i32>,
}

impl StateFile {
    fn save(&mut self, status: &str, exit_code: Option<i32>) {
        let record = Record {
            id: self.id,
            cmd: &self.cmd,
            started_unix: self.started_unix,
            cgroup: self.cgroup.clone(),
            init_pid: self.init_pid.and_then(pid_or_none),
            ip: self.ip.clone(),
            status,
            exit_code,
        };
        let body = match serde_json::to_string_pretty(&record) {
            Ok(b) => b,
            Err(_) => return,
        };
        let dir = state_dir();
        let path = dir.join(format!("{}.json", self.id));
        if let Err(e) = fs::create_dir_all(&dir).and_then(|_| fs::write(&path, body)) {
            eprintln!("minibox: warning: could not write {}: {e}", path.display());
        }
    }
}

/// Addresses held by runs that are still alive, read from their state records.
fn used_addresses(dir: &Path) -> Vec<String> {
    let Ok(entries) = fs::read_dir(dir) else {
        return Vec::new();
    };
    entries
        .flatten()
        .filter_map(|e| {
            let v: serde_json::Value = serde_json::from_slice(&fs::read(e.path()).ok()?).ok()?;
            let alive = v["init_pid"]
                .as_i64()
                .map(|p| Path::new(&format!("/proc/{p}")).exists())
                .unwrap_or(false);
            if v["status"] == "running" && alive {
                v["ip"].as_str().map(String::from)
            } else {
                None
            }
        })
        .collect()
}

fn state_dir() -> PathBuf {
    std::env::var_os("MINIBOX_STATE_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(DEFAULT_STATE_DIR))
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

// ---- signals and waiting ------------------------------------------------

/// Pid that signals are forwarded to: the sandbox's first process in the runner,
/// the init in stage1, and the real command in the init.
static FORWARD_TO: AtomicI32 = AtomicI32::new(0);
static SIGNALS_SEEN: AtomicU32 = AtomicU32::new(0);

extern "C" fn forward_signal(sig: libc::c_int) {
    // The first signal is forwarded as-is. The second one forces SIGKILL.
    let seen = SIGNALS_SEEN.fetch_add(1, Ordering::SeqCst);
    let pid = FORWARD_TO.load(Ordering::SeqCst);
    if pid > 0 {
        let s = if seen >= 1 { libc::SIGKILL } else { sig };
        // SAFETY: kill is async-signal-safe.
        unsafe {
            libc::kill(pid, s);
        }
    }
}

fn install_forwarders(target: Pid) {
    FORWARD_TO.store(target.as_raw(), Ordering::SeqCst);
    let handler: extern "C" fn(libc::c_int) = forward_signal;
    for sig in [libc::SIGINT, libc::SIGTERM] {
        // SAFETY: installing a plain C handler that only does atomic work and kill.
        unsafe {
            libc::signal(sig, handler as libc::sighandler_t);
        }
    }
}

/// Waits for the sandbox to finish. Returns its exit code and whether the timeout killed it.
/// Output is copied while it waits.
fn wait_child(
    child: Pid,
    timeout: Option<Duration>,
    out: &mut OutputPump,
) -> Result<(i32, bool), String> {
    install_forwarders(child);
    let deadline = timeout.map(|t| Instant::now() + t);
    loop {
        out.pump_once(5);
        match waitpid(child, Some(WaitPidFlag::WNOHANG)) {
            Ok(WaitStatus::Exited(_, code)) => return Ok((code, false)),
            Ok(WaitStatus::Signaled(_, sig, _)) => return Ok((128 + sig as i32, false)),
            Ok(WaitStatus::StillAlive) => {
                if let Some(d) = deadline {
                    if Instant::now() >= d {
                        // Killing stage1 also takes its init down (see PR_SET_PDEATHSIG).
                        let _ = kill(child, Signal::SIGKILL);
                        let _ = waitpid(child, None);
                        return Ok((TIMED_OUT, true));
                    }
                }
            }
            Ok(_) => continue,
            Err(Errno::EINTR) => continue,
            Err(e) => return Err(format!("waitpid: {e}")),
        }
    }
}

// ---- sandbox side -------------------------------------------------------
//
// The sandbox is built in three steps, each a process:
//   runner  (host)           : cgroups, state, veth, output
//   stage1  (new user ns)    : waits for the id mapping, makes the pid/uts/ipc/net namespaces
//   init    (new pid ns)     : PID 1. Mount namespace, rootfs, hardening, runs the command

struct StagePlan<'a> {
    cfg: &'a Config,
    argv: &'a [CString],
    root: Option<PathBuf>,
    down: OwnedFd,
    up: OwnedFd,
    stdout: OwnedFd,
    stderr: OwnedFd,
    runner_pid: i32,
}

struct InitPlan<'a> {
    cfg: &'a Config,
    argv: &'a [CString],
    root: Option<PathBuf>,
}

/// Runs in the forked stage1 process. Never returns.
fn stage1(plan: StagePlan) -> ! {
    let code = match stage1_run(plan) {
        Ok(code) => code,
        Err(e) => {
            eprintln!("minibox: setup failed: {e}");
            SETUP_FAILED
        }
    };
    std::process::exit(code)
}

fn stage1_run(plan: StagePlan) -> Result<i32, String> {
    let StagePlan {
        cfg,
        argv,
        root,
        down,
        up,
        stdout,
        stderr,
        runner_pid,
    } = plan;

    // Command output goes back to the runner.
    // SAFETY: plain dup2 onto the standard descriptors.
    unsafe {
        libc::dup2(stdout.as_raw_fd(), 1);
        libc::dup2(stderr.as_raw_fd(), 2);
    }
    drop((stdout, stderr));

    // If the runner dies, the sandbox dies with it.
    set_parent_death_signal(runner_pid)?;

    // The network namespace comes first, while this process still has host root. The
    // runner attaches the veth pair to it, and the job's user namespace never owns it.
    let mut up = File::from(up);
    let mut down = BufReader::new(File::from(down));
    if cfg.net != NetMode::Host {
        unshare(CloneFlags::CLONE_NEWNET).map_err(|e| format!("unshare network: {e}"))?;
    }
    writeln!(up, "net").map_err(|e| format!("report to runner: {e}"))?;
    let mut addr = String::new();
    down.read_line(&mut addr)
        .map_err(|e| format!("wait for address: {e}"))?;
    let addr = addr.trim().to_string();
    match cfg.net {
        NetMode::Bridge => {
            if addr.is_empty() || addr == "-" {
                return Err("runner sent no sandbox address".into());
            }
            net::configure_child(&addr)?;
        }
        NetMode::Isolated => bring_up_loopback()?,
        NetMode::Host => {}
    }

    unshare(CloneFlags::CLONE_NEWUSER).map_err(|e| format!("unshare user namespace: {e}"))?;
    writeln!(up, "userns").map_err(|e| format!("report to runner: {e}"))?;
    let mut line = String::new();
    down.read_line(&mut line)
        .map_err(|e| format!("wait for id mapping: {e}"))?;
    if line.trim() != "mapped" {
        return Err("runner did not map the sandbox ids".into());
    }

    unshare(CloneFlags::CLONE_NEWPID | CloneFlags::CLONE_NEWUTS | CloneFlags::CLONE_NEWIPC)
        .map_err(|e| format!("unshare namespaces: {e}"))?;

    // The first child after unshare(CLONE_NEWPID) becomes PID 1 of the sandbox.
    // SAFETY: still single-threaded.
    match unsafe { fork() }.map_err(|e| format!("fork init: {e}"))? {
        ForkResult::Child => init_main(InitPlan { cfg, argv, root }),
        ForkResult::Parent { child } => {
            install_forwarders(child);
            let code = loop {
                match waitpid(child, None) {
                    Ok(WaitStatus::Exited(_, c)) => break c,
                    Ok(WaitStatus::Signaled(_, sig, _)) => break 128 + sig as i32,
                    Ok(_) => continue,
                    Err(Errno::EINTR) => continue,
                    Err(e) => return Err(format!("wait for init: {e}")),
                }
            };
            Ok(code)
        }
    }
}

/// PID 1 inside the sandbox. Sets up its mounts and rootfs, hardens itself, then runs the
/// command. Never returns.
fn init_main(plan: InitPlan) -> ! {
    let code = match init_setup(&plan) {
        Ok(()) => SETUP_FAILED,
        Err(e) => {
            eprintln!("minibox: setup failed: {e}");
            SETUP_FAILED
        }
    };
    std::process::exit(code)
}

fn init_setup(plan: &InitPlan) -> Result<(), String> {
    // The init gets its own mount namespace, so pivot_root can't reach the runner.
    set_parent_death_signal(0)?;
    unshare(CloneFlags::CLONE_NEWNS).map_err(|e| format!("unshare mounts: {e}"))?;
    mount(
        None::<&str>,
        "/",
        None::<&str>,
        MsFlags::MS_REC | MsFlags::MS_PRIVATE,
        None::<&str>,
    )
    .map_err(|e| format!("make / private: {e}"))?;

    match &plan.root {
        Some(r) => enter_rootfs(r, plan.cfg)?,
        None => mount(
            Some("proc"),
            "/proc",
            Some("proc"),
            MsFlags::empty(),
            None::<&str>,
        )
        .map_err(|e| format!("mount /proc: {e}"))?,
    }
    harden()?;
    init_loop(plan.cfg, plan.argv)
}

/// Asks the kernel to send SIGKILL to this process when its parent dies. Pass the
/// runner's pid to also check it hasn't already gone, or 0 to skip that check.
fn set_parent_death_signal(expected_parent: i32) -> Result<(), String> {
    // SAFETY: prctl with constant arguments.
    let r = unsafe { libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL as libc::c_ulong, 0, 0, 0) };
    if r != 0 {
        return Err(format!("set parent death signal: {}", io::Error::last_os_error()));
    }
    if expected_parent > 0 {
        // SAFETY: getppid has no arguments and no side effects.
        let ppid = unsafe { libc::getppid() };
        if ppid != expected_parent {
            return Err("runner exited before the sandbox started".into());
        }
    }
    Ok(())
}

/// Mounts the copy-on-write root: the image as the read-only lower layer, the run's
/// upper layer on top for writes.
fn mount_overlay(o: &Overlay) -> Result<(), String> {
    for dir in [&o.upper, &o.work, &o.merged] {
        fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    }
    let opts = format!(
        "lowerdir={},upperdir={},workdir={}",
        o.lower.display(),
        o.upper.display(),
        o.work.display()
    );
    mount(
        Some("overlay"),
        o.merged.as_path(),
        Some("overlay"),
        MsFlags::empty(),
        Some(opts.as_str()),
    )
    .map_err(|e| format!("mount overlay: {e}"))
}

/// PID 1 in the sandbox. Forks the real command, reaps every child that
/// finishes (including orphans), and forwards signals to the command.
/// Exits the process once the command and all its children are gone.
fn init_loop(cfg: &Config, argv: &[CString]) -> Result<(), String> {
    // SAFETY: still single-threaded.
    match unsafe { fork() }.map_err(|e| format!("fork command: {e}"))? {
        ForkResult::Child => exec_command(cfg, argv),
        ForkResult::Parent { child } => {
            install_forwarders(child);
            let mut code = 1;
            loop {
                match waitpid(None, None) {
                    Ok(WaitStatus::Exited(pid, c)) if pid == child => code = c,
                    Ok(WaitStatus::Signaled(pid, sig, _)) if pid == child => {
                        code = 128 + sig as i32
                    }
                    Ok(_) => {}
                    Err(Errno::EINTR) => continue,
                    Err(Errno::ECHILD) => break,
                    Err(e) => return Err(format!("init wait: {e}")),
                }
            }
            std::process::exit(code)
        }
    }
}

/// Runs in the command's process. Sets up the environment and working directory, then execs.
fn exec_command(cfg: &Config, argv: &[CString]) -> ! {
    set_env(cfg);
    if let Some(dir) = &cfg.workdir {
        if let Err(e) = chdir(dir.as_path()) {
            eprintln!("minibox: workdir {}: {e}", dir.display());
            std::process::exit(126);
        }
    }
    if let Err(e) = become_job_user() {
        eprintln!("minibox: {e}");
        std::process::exit(126);
    }
    let err = execvp(argv[0].as_c_str(), argv).unwrap_err();
    eprintln!("minibox: exec {}: {err}", cfg.cmd[0]);
    std::process::exit(127);
}

/// Switches the command to the job's unprivileged host user. Inside the user namespace that
/// user is root, but it holds no capabilities, and the host sees it as BASE_ID.
fn become_job_user() -> Result<(), String> {
    // These calls take ids as the namespace sees them. Inside, the job's root is 0, which is
    // BASE_ID on the host. The change from the namespace-creating root to this user also
    // clears the capabilities.
    // SAFETY: plain credential syscalls with constant arguments.
    unsafe {
        if libc::setresgid(0, 0, 0) != 0 {
            return Err(format!("setresgid: {}", io::Error::last_os_error()));
        }
        if libc::setresuid(0, 0, 0) != 0 {
            return Err(format!("setresuid: {}", io::Error::last_os_error()));
        }
    }
    Ok(())
}

/// Replaces the inherited environment with a clean one: a default PATH, HOME, and the
/// variables given with -e. Host variables do not leak into the sandbox.
fn set_env(cfg: &Config) {
    let keys: Vec<_> = std::env::vars_os().map(|(k, _)| k).collect();
    for k in keys {
        std::env::remove_var(k);
    }
    std::env::set_var("PATH", DEFAULT_PATH);
    std::env::set_var("HOME", "/root");
    for (k, v) in &cfg.env {
        std::env::set_var(k, v);
    }
}

/// Pivots into `root` and applies the mounts that belong inside it: device nodes,
/// DNS config, and volumes.
fn enter_rootfs(root: &Path, cfg: &Config) -> Result<(), String> {
    mount(
        Some(root),
        root,
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REC,
        None::<&str>,
    )
    .map_err(|e| format!("bind rootfs: {e}"))?;

    // A few safe device nodes, bind-mounted from the host.
    // Minimal images may not ship /dev at all, so create the mount point.
    let dev = root.join("dev");
    fs::create_dir_all(&dev).map_err(|e| format!("create {}: {e}", dev.display()))?;
    for node in DEV_NODES {
        let target = dev.join(node);
        File::create(&target).map_err(|e| format!("create {}: {e}", target.display()))?;
        let host = Path::new("/dev").join(node);
        mount(
            Some(&host),
            &target,
            None::<&str>,
            MsFlags::MS_BIND,
            None::<&str>,
        )
        .map_err(|e| format!("bind {}: {e}", host.display()))?;
    }

    // Let the sandbox resolve names with the host's resolver config.
    if Path::new("/etc/resolv.conf").exists() {
        let target = root.join("etc/resolv.conf");
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
        }
        if !target.exists() {
            File::create(&target).map_err(|e| format!("create {}: {e}", target.display()))?;
        }
        mount(
            Some(Path::new("/etc/resolv.conf")),
            &target,
            None::<&str>,
            MsFlags::MS_BIND,
            None::<&str>,
        )
        .map_err(|e| format!("bind resolv.conf: {e}"))?;
    }

    for v in &cfg.volumes {
        let target = root.join(v.guest.strip_prefix("/").unwrap_or(&v.guest));
        bind_volume(v, &target)?;
    }

    let old = root.join(".old_root");
    fs::create_dir_all(&old).map_err(|e| format!("create {}: {e}", old.display()))?;
    pivot_root(root, &old).map_err(|e| format!("pivot_root: {e}"))?;
    chdir("/").map_err(|e| format!("chdir /: {e}"))?;
    umount2("/.old_root", MntFlags::MNT_DETACH).map_err(|e| format!("unmount old root: {e}"))?;
    fs::remove_dir("/.old_root").map_err(|e| format!("remove /.old_root: {e}"))?;

    fs::create_dir_all("/proc").map_err(|e| format!("create /proc: {e}"))?;
    mount(
        Some("proc"),
        "/proc",
        Some("proc"),
        MsFlags::empty(),
        None::<&str>,
    )
    .map_err(|e| format!("mount /proc: {e}"))?;
    Ok(())
}

/// Bind-mounts a host path onto `target`, read-only if asked.
fn bind_volume(v: &Volume, target: &Path) -> Result<(), String> {
    if v.host.is_dir() {
        fs::create_dir_all(target).map_err(|e| format!("create {}: {e}", target.display()))?;
    } else {
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
        }
        if !target.exists() {
            File::create(target).map_err(|e| format!("create {}: {e}", target.display()))?;
        }
    }
    mount(
        Some(&v.host),
        target,
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REC,
        None::<&str>,
    )
    .map_err(|e| format!("bind {} -> {}: {e}", v.host.display(), target.display()))?;
    if v.read_only {
        mount(
            None::<&str>,
            target,
            None::<&str>,
            MsFlags::MS_BIND | MsFlags::MS_REMOUNT | MsFlags::MS_RDONLY,
            None::<&str>,
        )
        .map_err(|e| format!("remount {} read-only: {e}", target.display()))?;
    }
    Ok(())
}

/// Brings `lo` up. A new network namespace starts with loopback down.
fn bring_up_loopback() -> Result<(), String> {
    // SAFETY: plain libc socket and ioctl calls with a zeroed ifreq.
    unsafe {
        let sock = libc::socket(libc::AF_INET, libc::SOCK_DGRAM, 0);
        if sock < 0 {
            return Err(format!("socket: {}", io::Error::last_os_error()));
        }
        let mut req: libc::ifreq = std::mem::zeroed();
        for (i, b) in b"lo".iter().enumerate() {
            req.ifr_name[i] = *b as libc::c_char;
        }
        let result = if libc::ioctl(sock, libc::SIOCGIFFLAGS, &mut req) < 0 {
            Err(format!("read lo flags: {}", io::Error::last_os_error()))
        } else {
            req.ifr_ifru.ifru_flags |= libc::IFF_UP as libc::c_short;
            if libc::ioctl(sock, libc::SIOCSIFFLAGS, &req) < 0 {
                Err(format!("bring up lo: {}", io::Error::last_os_error()))
            } else {
                Ok(())
            }
        };
        libc::close(sock);
        result
    }
}

/// Sets no_new_privs, clears the capability bounding set and installs a seccomp deny list.
fn harden() -> Result<(), String> {
    // SAFETY: prctl with these constant arguments has no memory effects.
    unsafe {
        if libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 {
            return Err(format!("no_new_privs: {}", io::Error::last_os_error()));
        }
        // Dropping an unknown capability returns EINVAL; that's fine past the last one.
        for cap in 0..=63 {
            libc::prctl(libc::PR_CAPBSET_DROP, cap as libc::c_ulong, 0, 0, 0);
        }
    }

    let mut rules = std::collections::BTreeMap::new();
    for nr in ALLOWED_SYSCALLS.iter().chain(LEGACY_SYSCALLS.iter()) {
        // An empty rule list matches the syscall for any arguments.
        rules.insert(*nr as i64, Vec::new());
    }
    let arch = if cfg!(target_arch = "aarch64") {
        TargetArch::aarch64
    } else {
        TargetArch::x86_64
    };
    // Calls in the list run. Everything else (the mismatch case) gets EPERM.
    let filter = SeccompFilter::new(
        rules,
        SeccompAction::Errno(libc::EPERM as u32),
        SeccompAction::Allow,
        arch,
    )
    .map_err(|e| format!("seccomp filter: {e}"))?;
    let program: BpfProgram = filter
        .try_into()
        .map_err(|e| format!("seccomp compile: {e}"))?;
    apply_filter(&program).map_err(|e| format!("seccomp apply: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn parses_basic_run() {
        let cfg = parse(&args(&["run", "--mem", "64", "--", "echo", "hi"])).unwrap();
        assert_eq!(cfg.mem_mb, Some(64));
        assert_eq!(cfg.cmd, vec!["echo", "hi"]);
    }

    #[test]
    fn parses_limits_and_image() {
        let cfg = parse(&args(&[
            "run", "--image", "ci", "--cpus", "0.5", "--timeout", "30", "-v", "/a:/b:ro", "--", "true",
        ]))
        .unwrap();
        assert_eq!(cfg.image.as_deref(), Some("ci"));
        assert_eq!(cfg.cpus, Some(0.5));
        assert_eq!(cfg.timeout_secs, Some(30));
        assert!(cfg.volumes[0].read_only);
    }

    #[test]
    fn parses_bridge_volume_and_env() {
        let cfg = parse(&args(&[
            "run", "--rootfs", "/r", "--net", "bridge", "-v", "/a:/b:ro", "-e", "K=V=W", "-w", "/b",
            "--", "true",
        ]))
        .unwrap();
        assert_eq!(cfg.net, NetMode::Bridge);
        assert_eq!(cfg.volumes[0].guest, PathBuf::from("/b"));
        assert_eq!(cfg.env, vec![("K".to_string(), "V=W".to_string())]);
        assert_eq!(cfg.workdir, Some(PathBuf::from("/b")));
    }

    #[test]
    fn rejects_bad_input() {
        assert!(parse(&args(&["run", "--"])).is_err());
        assert!(parse(&args(&["run", "--net", "vlan", "--", "true"])).is_err());
        assert!(parse(&args(&["run", "-v", "/a:/b", "--", "true"])).is_err());
        assert!(parse(&args(&["run", "--rootfs", "/r", "-v", "a:/b", "--", "true"])).is_err());
        assert!(parse(&args(&["run", "-e", "NOEQUALS", "--", "true"])).is_err());
        assert!(parse(&args(&["run", "--image", "x", "--rootfs", "/r", "--", "true"])).is_err());
        assert!(parse(&args(&["run", "--cpus", "0", "--", "true"])).is_err());
    }
}
