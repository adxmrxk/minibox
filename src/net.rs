//! Bridge networking: one Linux bridge (`minibox0`) with NAT for the whole host,
//! and one veth pair per sandbox. Shells out to `ip` and `iptables`.

use std::fs;
use std::process::Command;

pub const BRIDGE: &str = "minibox0";
pub const SUBNET_PREFIX: &str = "10.88.0";
pub const GATEWAY: &str = "10.88.0.1";

fn run_cmd(program: &str, args: &[&str]) -> Result<(), String> {
    let out = Command::new(program)
        .args(args)
        .output()
        .map_err(|e| format!("{program}: {e}"))?;
    if out.status.success() {
        Ok(())
    } else {
        Err(format!(
            "{program} {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        ))
    }
}

fn succeeds(program: &str, args: &[&str]) -> bool {
    Command::new(program)
        .args(args)
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

/// Creates the bridge, its gateway address, IP forwarding and the NAT rules, if missing.
/// Safe to call on every run.
pub fn ensure_bridge() -> Result<(), String> {
    if !succeeds("ip", &["link", "show", BRIDGE]) {
        run_cmd("ip", &["link", "add", BRIDGE, "type", "bridge"])?;
        run_cmd("ip", &["addr", "add", &format!("{GATEWAY}/24"), "dev", BRIDGE])?;
    }
    run_cmd("ip", &["link", "set", BRIDGE, "up"])?;
    fs::write("/proc/sys/net/ipv4/ip_forward", "1")
        .map_err(|e| format!("enable ip_forward: {e}"))?;

    let subnet = format!("{SUBNET_PREFIX}.0/24");
    ensure_rule(
        "nat",
        &["POSTROUTING", "-s", &subnet, "!", "-o", BRIDGE, "-j", "MASQUERADE"],
    )?;
    ensure_rule("filter", &["FORWARD", "-i", BRIDGE, "-j", "ACCEPT"])?;
    ensure_rule("filter", &["FORWARD", "-o", BRIDGE, "-j", "ACCEPT"])?;
    Ok(())
}

/// Appends `rule` to `chain` in `table` unless an identical rule already exists.
/// `rule[0]` is the chain; the rest are the match and target arguments.
fn ensure_rule(table: &str, rule: &[&str]) -> Result<(), String> {
    let (chain, rest) = (rule[0], &rule[1..]);

    let mut check: Vec<&str> = vec!["-t", table, "-C", chain];
    check.extend_from_slice(rest);
    if succeeds("iptables", &check) {
        return Ok(());
    }

    let mut add: Vec<&str> = vec!["-t", table, "-A", chain];
    add.extend_from_slice(rest);
    run_cmd("iptables", &add)
}

/// Name of the host end of the veth pair for a sandbox.
/// Interface names are limited to 15 characters: "mb" plus a pid fits.
pub fn host_if_name(init_pid: i32) -> String {
    format!("mb{init_pid}")
}

/// Creates the veth pair, moves the peer into the sandbox's network namespace as `eth0`,
/// and plugs the host end into the bridge. `pid` is a process inside that namespace.
pub fn attach(pid: i32) -> Result<(), String> {
    let host_if = host_if_name(pid);
    let pid = pid.to_string();
    run_cmd(
        "ip",
        &["link", "add", &host_if, "type", "veth", "peer", "name", "eth0", "netns", &pid],
    )?;
    run_cmd("ip", &["link", "set", &host_if, "master", BRIDGE])?;
    run_cmd("ip", &["link", "set", &host_if, "up"])
}

/// Runs inside the sandbox's network namespace, before pivot_root.
/// Brings up loopback and eth0 with the address the parent picked.
pub fn configure_child(addr: &str) -> Result<(), String> {
    run_cmd("ip", &["link", "set", "lo", "up"])?;
    run_cmd("ip", &["addr", "add", &format!("{addr}/24"), "dev", "eth0"])?;
    run_cmd("ip", &["link", "set", "eth0", "up"])?;
    run_cmd("ip", &["route", "add", "default", "via", GATEWAY])
}

/// First address in the subnet that is neither the gateway nor already in use.
pub fn pick_address(used: &[String]) -> Option<String> {
    (2..=254)
        .map(|n| format!("{SUBNET_PREFIX}.{n}"))
        .find(|addr| !used.iter().any(|u| u == addr))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn picks_first_free_address() {
        assert_eq!(pick_address(&[]), Some("10.88.0.2".to_string()));
        let used = vec!["10.88.0.2".to_string(), "10.88.0.3".to_string()];
        assert_eq!(pick_address(&used), Some("10.88.0.4".to_string()));
    }

    #[test]
    fn never_hands_out_the_gateway() {
        assert!(!(2..=254).any(|n| format!("{SUBNET_PREFIX}.{n}") == GATEWAY));
    }

    #[test]
    fn interface_name_fits_kernel_limit() {
        assert!(host_if_name(4_194_304).len() <= 15);
    }
}
