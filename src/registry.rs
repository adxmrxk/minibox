//! Pulls images from a Docker Registry v2 server (Docker Hub, GHCR, or a private registry).
//!
//! Steps: fetch the manifest (following a platform index to the linux build for this CPU),
//! download each layer, check its SHA-256 digest, and apply the layers in order. Whiteout
//! files (.wh.NAME deletes NAME, .wh..wh..opq clears a directory) are honoured.

use std::fs::{self, File};
use std::io;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::linux::{image_dir, BASE_ID};

const MANIFEST_ACCEPT: &str = "application/vnd.oci.image.index.v1+json, \
application/vnd.docker.distribution.manifest.list.v2+json, \
application/vnd.oci.image.manifest.v1+json, \
application/vnd.docker.distribution.manifest.v2+json";

const DEFAULT_REGISTRY: &str = "registry-1.docker.io";

#[derive(Debug, PartialEq, Eq)]
struct Reference {
    registry: String,
    repo: String,
    tag: String,
}

/// Parses "alpine:3.20", "library/alpine", "ghcr.io/org/app:v1" and "localhost:5000/app".
fn parse_reference(input: &str) -> Result<Reference, String> {
    let (name, tag) = match input.rsplit_once(':') {
        Some((n, t)) if !t.contains('/') && !t.is_empty() => (n, t),
        _ => (input, "latest"),
    };
    let parts: Vec<&str> = name.split('/').collect();
    let (registry, repo) = if parts.len() > 1
        && (parts[0].contains('.') || parts[0].contains(':') || parts[0] == "localhost")
    {
        (parts[0].to_string(), parts[1..].join("/"))
    } else if parts.len() == 1 {
        (DEFAULT_REGISTRY.to_string(), format!("library/{name}"))
    } else {
        (DEFAULT_REGISTRY.to_string(), name.to_string())
    };
    // Repository names are lowercase. Tags may also use upper case.
    let repo_ok = !repo.is_empty()
        && repo.chars().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || "._-/".contains(c));
    let tag_ok = !tag.is_empty() && tag.chars().all(|c| c.is_ascii_alphanumeric() || "._-".contains(c));
    if !repo_ok || !tag_ok {
        return Err(format!("bad image reference '{input}'"));
    }
    Ok(Reference {
        registry,
        repo,
        tag: tag.to_string(),
    })
}

/// Pulls `reference` from its registry and stores it as image `name`.
pub fn pull(name: &str, reference: &str) -> Result<(), String> {
    if name.is_empty() || name.contains('/') || name.starts_with('.') {
        return Err(format!("bad image name '{name}'"));
    }
    let dest = image_dir().join(name);
    if dest.exists() {
        return Err(format!("image '{name}' already exists"));
    }
    let r = parse_reference(reference)?;
    let base = format!("https://{}/v2/{}", r.registry, r.repo);
    let arch = match std::env::consts::ARCH {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        other => return Err(format!("no registry platform mapping for {other}")),
    };

    let mut token: Option<String> = None;
    let mut manifest = get_json(&format!("{base}/manifests/{}", r.tag), MANIFEST_ACCEPT, &mut token)?;

    // A multi-architecture index points at one manifest per platform. Pick linux/<arch>.
    if let Some(list) = manifest["manifests"].as_array() {
        let digest = list
            .iter()
            .find(|m| {
                m["platform"]["os"] == "linux" && m["platform"]["architecture"] == arch
            })
            .and_then(|m| m["digest"].as_str())
            .ok_or_else(|| format!("no linux/{arch} build in {reference}"))?
            .to_string();
        manifest = get_json(&format!("{base}/manifests/{digest}"), MANIFEST_ACCEPT, &mut token)?;
    }

    let layers: Vec<String> = manifest["layers"]
        .as_array()
        .ok_or("manifest has no layers")?
        .iter()
        .filter_map(|l| l["digest"].as_str().map(String::from))
        .collect();
    if layers.is_empty() {
        return Err("manifest has no layers".into());
    }

    let stamp = unique();
    let staging = image_dir().join(format!(".pull-{name}-{stamp}"));
    let blob = std::env::temp_dir().join(format!("minibox-blob-{stamp}"));
    fs::create_dir_all(&staging).map_err(|e| format!("create {}: {e}", staging.display()))?;

    let result = (|| -> Result<(), String> {
        for digest in &layers {
            let url = format!("{base}/blobs/{digest}");
            get_to_file(&url, &mut token, &blob)?;
            verify_digest(&blob, digest)?;
            apply_layer(&blob, &staging)?;
            let _ = fs::remove_file(&blob);
        }
        // Everything in the image belongs to the job's root.
        let owner = format!("{BASE_ID}:{BASE_ID}");
        let status = Command::new("chown")
            .args(["-R", &owner])
            .arg(&staging)
            .status()
            .map_err(|e| format!("run chown: {e}"))?;
        if !status.success() {
            return Err("chown of the pulled image failed".into());
        }
        fs::rename(&staging, &dest).map_err(|e| format!("install image: {e}"))?;
        println!("pulled {reference} as {name} ({} layers)", layers.len());
        Ok(())
    })();

    let _ = fs::remove_file(&blob);
    if result.is_err() {
        let _ = fs::remove_dir_all(&staging);
    }
    result
}

struct Response {
    status: u32,
    body: Vec<u8>,
    www_auth: Option<String>,
}

/// One HTTP GET through curl. The body goes to `body_file` if given, otherwise into memory.
fn http_get(url: &str, headers: &[(&str, String)], body_file: Option<&Path>) -> Result<Response, String> {
    let stamp = unique();
    let hdr = std::env::temp_dir().join(format!("minibox-hdr-{stamp}"));
    let body = match body_file {
        Some(p) => p.to_path_buf(),
        None => std::env::temp_dir().join(format!("minibox-body-{stamp}")),
    };
    let mut cmd = Command::new("curl");
    cmd.args(["-sSL", "--max-time", "900", "-D"])
        .arg(&hdr)
        .arg("-o")
        .arg(&body)
        .args(["-w", "%{http_code}"]);
    // A registry with a private certificate authority: MINIBOX_REGISTRY_CA points at its PEM file.
    if let Ok(ca) = std::env::var("MINIBOX_REGISTRY_CA") {
        cmd.arg("--cacert").arg(ca);
    }
    for (k, v) in headers {
        cmd.arg("-H").arg(format!("{k}: {v}"));
    }
    cmd.arg(url);
    let out = cmd.output().map_err(|e| format!("run curl: {e}"))?;

    let header_text = fs::read_to_string(&hdr).unwrap_or_default();
    let _ = fs::remove_file(&hdr);
    if !out.status.success() {
        let _ = fs::remove_file(&body);
        return Err(format!(
            "request to {url} failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    let status: u32 = String::from_utf8_lossy(&out.stdout).trim().parse().unwrap_or(0);
    // With redirects curl prints several header blocks. The last one is the answer.
    let www_auth = header_text
        .lines()
        .filter(|l| l.to_ascii_lowercase().starts_with("www-authenticate:"))
        .last()
        .map(|l| l.split_once(':').map(|(_, v)| v.trim().to_string()).unwrap_or_default());
    let data = if body_file.is_none() {
        let data = fs::read(&body).unwrap_or_default();
        let _ = fs::remove_file(&body);
        data
    } else {
        Vec::new()
    };
    Ok(Response {
        status,
        body: data,
        www_auth,
    })
}

/// GET with registry token auth. On a 401 it fetches a bearer token from the realm the
/// registry names, then retries once. The token is kept for later requests.
fn authed_get(
    url: &str,
    accept: &str,
    token: &mut Option<String>,
    body_file: Option<&Path>,
) -> Result<Response, String> {
    let headers = |token: &Option<String>| {
        let mut h = vec![("Accept", accept.to_string())];
        if let Some(t) = token {
            h.push(("Authorization", format!("Bearer {t}")));
        }
        h
    };
    let mut resp = http_get(url, &headers(token), body_file)?;
    if resp.status == 401 {
        let www = resp
            .www_auth
            .clone()
            .ok_or_else(|| format!("{url} returned 401 without a way to log in"))?;
        *token = Some(fetch_token(&www)?);
        resp = http_get(url, &headers(token), body_file)?;
    }
    if resp.status != 200 {
        return Err(format!("{url} returned HTTP {}", resp.status));
    }
    Ok(resp)
}

fn get_json(url: &str, accept: &str, token: &mut Option<String>) -> Result<Value, String> {
    let resp = authed_get(url, accept, token, None)?;
    serde_json::from_slice(&resp.body).map_err(|e| format!("bad JSON from {url}: {e}"))
}

fn get_to_file(url: &str, token: &mut Option<String>, path: &Path) -> Result<(), String> {
    authed_get(url, "*/*", token, Some(path)).map(|_| ())
}

/// Turns `Bearer realm="...",service="...",scope="..."` into a token.
fn fetch_token(www: &str) -> Result<String, String> {
    let params = www
        .strip_prefix("Bearer ")
        .ok_or("registry asks for an unsupported login method")?;
    let mut realm = None;
    let mut service = None;
    let mut scope = None;
    for part in params.split(',') {
        if let Some((k, v)) = part.trim().split_once('=') {
            let v = v.trim_matches('"').to_string();
            match k {
                "realm" => realm = Some(v),
                "service" => service = Some(v),
                "scope" => scope = Some(v),
                _ => {}
            }
        }
    }
    let realm = realm.ok_or("login challenge has no realm")?;
    let mut url = realm;
    let mut sep = '?';
    for (k, v) in [("service", service), ("scope", scope)] {
        if let Some(v) = v {
            url.push(sep);
            url.push_str(&format!("{k}={}", percent_encode(&v)));
            sep = '&';
        }
    }
    // Private registries take a username and password on the token request.
    // Set MINIBOX_REGISTRY_USER and MINIBOX_REGISTRY_PASSWORD to use them.
    let mut headers = Vec::new();
    if let (Ok(user), Ok(pass)) = (
        std::env::var("MINIBOX_REGISTRY_USER"),
        std::env::var("MINIBOX_REGISTRY_PASSWORD"),
    ) {
        use base64::Engine;
        let creds = base64::engine::general_purpose::STANDARD.encode(format!("{user}:{pass}"));
        headers.push(("Authorization", format!("Basic {creds}")));
    }
    let resp = http_get(&url, &headers, None)?;
    if resp.status != 200 {
        return Err(format!("token request returned HTTP {} (check the registry credentials)", resp.status));
    }
    let json: Value = serde_json::from_slice(&resp.body).map_err(|e| format!("bad token JSON: {e}"))?;
    json["token"]
        .as_str()
        .or_else(|| json["access_token"].as_str())
        .map(String::from)
        .ok_or_else(|| "token response had no token".into())
}

fn percent_encode(s: &str) -> String {
    s.chars()
        .map(|c| match c {
            'a'..='z' | 'A'..='Z' | '0'..='9' | '-' | '_' | '.' | '~' => c.to_string(),
            _ => format!("%{:02X}", c as u32),
        })
        .collect()
}

/// Checks that the downloaded blob hashes to the digest the manifest names.
fn verify_digest(path: &Path, digest: &str) -> Result<(), String> {
    let want = digest
        .strip_prefix("sha256:")
        .ok_or_else(|| format!("unsupported digest {digest}"))?;
    let mut file = File::open(path).map_err(|e| format!("open blob: {e}"))?;
    let mut hasher = <Sha256 as Digest>::new();
    io::copy(&mut file, &mut hasher).map_err(|e| format!("hash blob: {e}"))?;
    let got = format!("{:x}", Digest::finalize(hasher));
    if got != want {
        return Err(format!("layer digest mismatch: expected {want}, got {got}"));
    }
    Ok(())
}

/// Applies one layer tarball on top of the staging directory, handling whiteouts first.
fn apply_layer(layer: &Path, staging: &Path) -> Result<(), String> {
    let listing = Command::new("tar")
        .arg("-tf")
        .arg(layer)
        .output()
        .map_err(|e| format!("run tar: {e}"))?;
    if !listing.status.success() {
        return Err("a layer is not a readable tar archive".into());
    }
    for line in String::from_utf8_lossy(&listing.stdout).lines() {
        let path = line.trim_start_matches("./").trim_end_matches('/');
        if path.is_empty() {
            continue;
        }
        let (dir, base) = match path.rsplit_once('/') {
            Some((d, b)) => (d, b),
            None => ("", path),
        };
        if base == ".wh..wh..opq" {
            // Opaque whiteout: the directory's lower contents are hidden.
            if let Ok(entries) = fs::read_dir(staging.join(dir)) {
                for e in entries.flatten() {
                    let _ = remove_path(&e.path());
                }
            }
        } else if let Some(original) = base.strip_prefix(".wh.") {
            // Whiteout: delete the file or directory it names.
            let _ = remove_path(&staging.join(dir).join(original));
        }
    }

    let status = Command::new("tar")
        .arg("-xf")
        .arg(layer)
        .arg("-C")
        .arg(staging)
        .args(["--no-same-owner", "--exclude=*/.wh.*", "--exclude=.wh.*"])
        .status()
        .map_err(|e| format!("run tar: {e}"))?;
    if !status.success() {
        return Err("failed to extract a layer".into());
    }
    Ok(())
}

fn remove_path(path: &PathBuf) -> io::Result<()> {
    let meta = fs::symlink_metadata(path)?;
    if meta.is_dir() {
        fs::remove_dir_all(path)
    } else {
        fs::remove_file(path)
    }
}

fn unique() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("{}-{nanos}", std::process::id())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn short_names_go_to_docker_hub_library() {
        let r = parse_reference("alpine:3.20").unwrap();
        assert_eq!(r.registry, DEFAULT_REGISTRY);
        assert_eq!(r.repo, "library/alpine");
        assert_eq!(r.tag, "3.20");
    }

    #[test]
    fn explicit_registries_are_kept() {
        let r = parse_reference("ghcr.io/org/app:v1").unwrap();
        assert_eq!(r.registry, "ghcr.io");
        assert_eq!(r.repo, "org/app");
        let l = parse_reference("localhost:5000/team/tool").unwrap();
        assert_eq!(l.registry, "localhost:5000");
        assert_eq!(l.tag, "latest");
    }

    #[test]
    fn rejects_strange_references() {
        assert!(parse_reference("bad name").is_err());
        assert!(parse_reference("UPPER/case").is_err());
    }
}
