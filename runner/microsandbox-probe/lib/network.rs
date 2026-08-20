//! Real-guest and policy-engine network qualification proof.

use std::net::{IpAddr, SocketAddr};
use std::path::Path;
use std::time::Duration;

use microsandbox::{NetworkPolicy, Sandbox};
use microsandbox_network::policy::Protocol;
use microsandbox_network::shared::{ResolvedHostnameFamily, SharedState};

/// Bounded evidence from deny-all, domain/port allow-list, and DNS-change checks.
#[derive(Debug, Eq, PartialEq)]
pub struct NetworkPolicyProof {
    /// Bytes returned by the allowed domain/port request.
    pub allowed_bytes: usize,
    /// Whether a disjoint domain was denied in the live guest.
    pub denied_domain: bool,
    /// Whether a private-address target was denied in the live guest.
    pub denied_private: bool,
    /// Whether the metadata target was denied in the live guest.
    pub denied_metadata: bool,
    /// Whether the no-network sandbox denied a public request.
    pub deny_all: bool,
    /// Whether replacing a cached DNS answer revoked the old address and admitted the new one.
    pub dns_change: bool,
}

/// Prove the exact default-deny translation in the evaluator and in a real KVM guest.
pub async fn run_network_policy_proof(
    work_dir: &Path,
    rootfs: &Path,
) -> Result<NetworkPolicyProof, String> {
    if !rootfs.is_dir() {
        return Err(format!("rootfs is not a directory: {}", rootfs.display()));
    }
    std::fs::create_dir(work_dir)
        .map_err(|error| format!("create exclusive network proof directory: {error}"))?;

    let policy = NetworkPolicy::builder()
        .default_deny()
        .egress(|rule| rule.tcp().port(80).allow().domain("example.com"))
        .build()
        .map_err(|error| format!("build representative allow-list: {error}"))?;
    let dns_change = prove_dns_change_and_sensitive_targets(&policy)?;

    let allow_name = format!("secondbox-task0l-net-{}", std::process::id());
    let allowed = Sandbox::builder(&allow_name)
        .image(rootfs.to_path_buf())
        .cpus(1)
        .memory(384)
        .disable_metrics_sample()
        .network(|network| network.policy(policy))
        .replace()
        .create()
        .await
        .map_err(|error| format!("create allow-list sandbox: {error}"))?;

    let response = allowed
        .shell("wget -T 15 -qO- http://example.com/")
        .await
        .map_err(|error| format!("allowed domain request: {error}"))?;
    if !response.status().success || response.stdout_bytes().is_empty() {
        return Err(format!(
            "allowed domain request failed: code={} stderr={}",
            response.status().code,
            response
                .stderr()
                .unwrap_or_default()
                .chars()
                .take(512)
                .collect::<String>()
        ));
    }
    let allowed_bytes = response.stdout_bytes().len();
    let denied_domain = command_is_denied(&allowed, "wget -T 3 -qO- http://example.net/").await?;
    let denied_private = command_is_denied(&allowed, "wget -T 3 -qO- http://10.0.0.1/").await?;
    let denied_metadata = command_is_denied(
        &allowed,
        "wget -T 3 -qO- http://169.254.169.254/latest/meta-data/",
    )
    .await?;
    allowed
        .stop()
        .await
        .map_err(|error| format!("stop allow-list sandbox: {error}"))?;
    Sandbox::get(&allow_name)
        .await
        .map_err(|error| format!("get stopped allow-list sandbox: {error}"))?
        .remove()
        .await
        .map_err(|error| format!("remove allow-list sandbox: {error}"))?;

    let deny_name = format!("secondbox-task0l-deny-{}", std::process::id());
    let denied = Sandbox::builder(&deny_name)
        .image(rootfs.to_path_buf())
        .cpus(1)
        .memory(384)
        .disable_metrics_sample()
        .disable_network()
        .replace()
        .create()
        .await
        .map_err(|error| format!("create deny-all sandbox: {error}"))?;
    let deny_all = command_is_denied(&denied, "wget -T 3 -qO- http://example.com/").await?;
    denied
        .stop()
        .await
        .map_err(|error| format!("stop deny-all sandbox: {error}"))?;
    Sandbox::get(&deny_name)
        .await
        .map_err(|error| format!("get stopped deny-all sandbox: {error}"))?
        .remove()
        .await
        .map_err(|error| format!("remove deny-all sandbox: {error}"))?;

    if !(denied_domain && denied_private && denied_metadata && deny_all && dns_change) {
        return Err("one or more mandatory network denial proofs did not deny".to_string());
    }
    Ok(NetworkPolicyProof {
        allowed_bytes,
        denied_domain,
        denied_private,
        denied_metadata,
        deny_all,
        dns_change,
    })
}

fn prove_dns_change_and_sensitive_targets(policy: &NetworkPolicy) -> Result<bool, String> {
    let shared = SharedState::new(8);
    let old_ip: IpAddr = "93.184.216.34"
        .parse()
        .map_err(|error| format!("parse old IP: {error}"))?;
    let new_ip: IpAddr = "93.184.216.35"
        .parse()
        .map_err(|error| format!("parse new IP: {error}"))?;
    shared.cache_resolved_hostname(
        "example.com",
        ResolvedHostnameFamily::Ipv4,
        [old_ip],
        Duration::from_secs(60),
    );
    let old_allowed = policy
        .evaluate_egress(SocketAddr::new(old_ip, 80), Protocol::Tcp, &shared)
        .is_allow();
    shared.clear_resolved_hostname("example.com", ResolvedHostnameFamily::Ipv4);
    shared.cache_resolved_hostname(
        "example.com",
        ResolvedHostnameFamily::Ipv4,
        [new_ip],
        Duration::from_secs(60),
    );
    let old_revoked = policy
        .evaluate_egress(SocketAddr::new(old_ip, 80), Protocol::Tcp, &shared)
        .is_deny();
    let new_allowed = policy
        .evaluate_egress(SocketAddr::new(new_ip, 80), Protocol::Tcp, &shared)
        .is_allow();
    let private_denied = policy
        .evaluate_egress("10.0.0.1:80".parse().unwrap(), Protocol::Tcp, &shared)
        .is_deny();
    let metadata_denied = policy
        .evaluate_egress(
            "169.254.169.254:80".parse().unwrap(),
            Protocol::Tcp,
            &shared,
        )
        .is_deny();
    let deny_all = NetworkPolicy::none()
        .evaluate_egress("8.8.8.8:53".parse().unwrap(), Protocol::Udp, &shared)
        .is_deny();
    Ok(old_allowed && old_revoked && new_allowed && private_denied && metadata_denied && deny_all)
}

async fn command_is_denied(sandbox: &Sandbox, command: &str) -> Result<bool, String> {
    let output = sandbox
        .shell(command)
        .await
        .map_err(|error| format!("run expected-denial command {command:?}: {error}"))?;
    Ok(!output.status().success)
}
