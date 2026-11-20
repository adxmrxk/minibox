//! minibox: run a command inside its own PID, mount, UTS, IPC and (optionally)
//! network namespaces, with cgroup v2 limits, a root filesystem, bridge networking,
//! volumes and a seccomp deny list. Linux only.

#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "linux")]
mod net;
#[cfg(target_os = "linux")]
mod registry;

#[cfg(target_os = "linux")]
fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    std::process::exit(linux::run_cli(args));
}

#[cfg(not(target_os = "linux"))]
fn main() {
    eprintln!("minibox needs Linux kernel features. On Windows, run it inside WSL2.");
    std::process::exit(2);
}
