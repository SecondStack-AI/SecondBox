//! Exact translation from the private helper start request to libkrun.

use std::path::PathBuf;

use msb_krun::{DiskImageFormat, SyncMode, VmBuilder};
use thiserror::Error;

use crate::protocol::{
    HelperNetworkDestination, HelperNetworkPolicyMode, HelperNetworkProtocol, StartRequest,
    helper_network_destination::Target,
};

pub const WORKSPACE_DESCRIPTOR_PATH: &str = "/proc/self/fd/4";
pub const GUEST_AGENTD_PATH: &str = "/init.secondbox-agentd";
const MIB: u64 = 1024 * 1024;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LaunchConfiguration {
    pub root: PathBuf,
    pub libkrunfw: PathBuf,
    pub agentd: PathBuf,
    pub vcpus: u8,
    pub memory_mib: usize,
    pub environment: Vec<(String, String)>,
    pub network: TranslatedNetworkPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TranslatedNetworkPolicy {
    DenyAll,
    AllowList(Vec<TranslatedNetworkDestination>),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TranslatedNetworkDestination {
    pub target: String,
    pub is_domain: bool,
    pub protocol: HelperNetworkProtocol,
    pub port: u16,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum LaunchError {
    #[error("SecondBox Microsandbox helper materialization identity is invalid")]
    Materialization,
    #[error("SecondBox Microsandbox helper flat root artifact is invalid")]
    Root,
    #[error("SecondBox Microsandbox helper execution asset is invalid")]
    Asset,
    #[error("SecondBox Microsandbox helper architecture is incompatible with this host")]
    Architecture,
    #[error("SecondBox Microsandbox helper VCPU count is invalid")]
    Vcpu,
    #[error("SecondBox Microsandbox helper memory size is invalid")]
    Memory,
    #[error("SecondBox Microsandbox helper startup environment is invalid")]
    Environment,
    #[error("SecondBox Microsandbox helper network policy is invalid")]
    Network,
    #[error("SecondBox Microsandbox helper Workspace attachment identity is invalid")]
    Workspace,
}

impl LaunchConfiguration {
    pub fn from_start(start: &StartRequest) -> Result<Self, LaunchError> {
        if !is_sha256(&start.materialization_digest) {
            return Err(LaunchError::Materialization);
        }
        if !is_sha256(&start.flat_root_digest) {
            return Err(LaunchError::Root);
        }
        let root = PathBuf::from(&start.flat_root_path);
        if !root.is_absolute() || !root.is_dir() {
            return Err(LaunchError::Root);
        }
        let libkrunfw = validate_asset(&start.libkrunfw_path)?;
        let agentd = validate_asset(&start.agentd_path)?;
        if !architecture_matches(&start.guest_architecture) {
            return Err(LaunchError::Architecture);
        }
        let vcpus = u8::try_from(start.vcpu_count).map_err(|_| LaunchError::Vcpu)?;
        if vcpus == 0 {
            return Err(LaunchError::Vcpu);
        }
        if start.memory_bytes == 0 || !start.memory_bytes.is_multiple_of(MIB) {
            return Err(LaunchError::Memory);
        }
        let memory_mib =
            usize::try_from(start.memory_bytes / MIB).map_err(|_| LaunchError::Memory)?;
        let environment = translate_environment(&start.environment)?;
        let network = translate_network(start)?;
        if start.stable_workspace_block_id != "workspace"
            || start.workspace_capacity_bytes == 0
            || start.workspace_uuid.len() != 16
        {
            return Err(LaunchError::Workspace);
        }
        Ok(Self {
            root,
            libkrunfw,
            agentd,
            vcpus,
            memory_mib,
            environment,
            network,
        })
    }

    /// Build the complete libkrun configuration without starting its non-returning VMM loop.
    pub fn build(
        self,
        console: crate::console::AgentConsoleBackend,
        runtime_dir: &std::path::Path,
    ) -> Result<msb_krun::Vm, msb_krun::Error> {
        self.build_with_workspace(
            PathBuf::from(WORKSPACE_DESCRIPTOR_PATH),
            Some(console),
            Some(runtime_dir.to_path_buf()),
        )
    }

    fn build_with_workspace(
        self,
        workspace_path: PathBuf,
        console: Option<crate::console::AgentConsoleBackend>,
        runtime_dir: Option<PathBuf>,
    ) -> Result<msb_krun::Vm, msb_krun::Error> {
        let root = self.root;
        let libkrunfw = self.libkrunfw;
        let agentd = self.agentd;
        let vcpus = self.vcpus;
        let memory_mib = self.memory_mib;
        let environment = self.environment;
        let mut builder = VmBuilder::new()
            .machine(|machine| {
                let machine = machine
                    .vcpus(vcpus)
                    .memory_mib(memory_mib)
                    .max_vcpus(vcpus)
                    .max_memory_mib(memory_mib)
                    .enable_inet_hijack(false)
                ;
                #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
                { machine.split_irqchip(true) }
                #[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
                { machine }
            })
            .kernel(|kernel| {
                let _validated_host_agent = agentd;
                kernel.krunfw_path(libkrunfw).init_path(GUEST_AGENTD_PATH)
            })
            .fs(|filesystem| filesystem.root(root))
            .disk(|disk| {
                disk.path(workspace_path)
                    .format(DiskImageFormat::Raw)
                    .id("workspace")
                    .read_only(false)
                    .sync(SyncMode::Full)
            })
            .exec(|exec| {
                exec.env("MSB_DISK_MOUNTS", "workspace:/workspace:fstype=ext4")
                    .envs(environment)
            });
        if let Some(runtime_dir) = runtime_dir {
            builder = builder.fs(|filesystem| filesystem.tag("msb_runtime").path(runtime_dir));
        }
        if let Some(console) = console {
            builder = builder.console(|ports| {
                ports
                    .output("/proc/self/fd/6")
                    .custom("agent", Box::new(console))
            });
        }
        builder.build()
    }
}

fn validate_asset(value: &str) -> Result<PathBuf, LaunchError> {
    let path = PathBuf::from(value);
    if !path.is_absolute() || !path.is_file() {
        return Err(LaunchError::Asset);
    }
    Ok(path)
}

fn translate_environment(values: &[String]) -> Result<Vec<(String, String)>, LaunchError> {
    values
        .iter()
        .map(|value| {
            let (key, value) = value.split_once('=').ok_or(LaunchError::Environment)?;
            if key.is_empty()
                || !key
                    .bytes()
                    .all(|byte| byte == b'_' || byte.is_ascii_alphanumeric())
                || key.as_bytes()[0].is_ascii_digit()
                || !value.is_ascii()
                || value.bytes().any(|byte| byte.is_ascii_control())
            {
                return Err(LaunchError::Environment);
            }
            Ok((key.to_owned(), value.to_owned()))
        })
        .collect()
}

fn translate_network(start: &StartRequest) -> Result<TranslatedNetworkPolicy, LaunchError> {
    let policy = start.network_policy.as_ref().ok_or(LaunchError::Network)?;
    match HelperNetworkPolicyMode::try_from(policy.mode).map_err(|_| LaunchError::Network)? {
        HelperNetworkPolicyMode::DenyAll if policy.destinations.is_empty() => {
            Ok(TranslatedNetworkPolicy::DenyAll)
        }
        HelperNetworkPolicyMode::AllowList if !policy.destinations.is_empty() => policy
            .destinations
            .iter()
            .map(translate_destination)
            .collect::<Result<Vec<_>, _>>()
            .map(TranslatedNetworkPolicy::AllowList),
        _ => Err(LaunchError::Network),
    }
}

fn translate_destination(
    destination: &HelperNetworkDestination,
) -> Result<TranslatedNetworkDestination, LaunchError> {
    let (target, is_domain) = match destination.target.as_ref() {
        Some(Target::Domain(value)) if !value.is_empty() => (value.clone(), true),
        Some(Target::Cidr(value)) if !value.is_empty() => (value.clone(), false),
        _ => return Err(LaunchError::Network),
    };
    let protocol =
        HelperNetworkProtocol::try_from(destination.protocol).map_err(|_| LaunchError::Network)?;
    if protocol == HelperNetworkProtocol::Unspecified {
        return Err(LaunchError::Network);
    }
    let port = u16::try_from(destination.port).map_err(|_| LaunchError::Network)?;
    if port == 0 {
        return Err(LaunchError::Network);
    }
    Ok(TranslatedNetworkDestination {
        target,
        is_domain,
        protocol,
        port,
    })
}

fn is_sha256(value: &str) -> bool {
    value.len() == 71
        && value.starts_with("sha256:")
        && value[7..].bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn architecture_matches(guest: &str) -> bool {
    matches!(
        (guest, std::env::consts::ARCH),
        ("amd64", "x86_64") | ("arm64", "aarch64")
    )
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use tempfile::tempdir;

    use super::*;
    use crate::protocol::{HelperNetworkPolicy, StartRequest};

    fn valid_start(root: &Path) -> StartRequest {
        StartRequest {
            materialization_digest: format!("sha256:{}", "a".repeat(64)),
            guest_architecture: if cfg!(target_arch = "x86_64") {
                "amd64".into()
            } else {
                "arm64".into()
            },
            vcpu_count: 2,
            memory_bytes: 512 * MIB,
            flat_root_digest: format!("sha256:{}", "b".repeat(64)),
            environment: vec!["PATH=/usr/bin".into(), "SECONDBOX_AGENT=1".into()],
            network_policy: Some(HelperNetworkPolicy {
                mode: HelperNetworkPolicyMode::DenyAll as i32,
                destinations: Vec::new(),
            }),
            stable_workspace_block_id: "workspace".into(),
            workspace_capacity_bytes: 64 * MIB,
            workspace_uuid: vec![0x42; 16],
            flat_root_path: root.display().to_string(),
            libkrunfw_path: "/bin/true".into(),
            agentd_path: "/bin/true".into(),
        }
    }

    #[test]
    fn translates_exact_launch_configuration() {
        let root = tempdir().unwrap();
        let launch = LaunchConfiguration::from_start(&valid_start(root.path())).unwrap();
        assert_eq!(launch.vcpus, 2);
        assert_eq!(launch.memory_mib, 512);
        assert_eq!(launch.network, TranslatedNetworkPolicy::DenyAll);
        assert_eq!(
            launch.environment[1],
            ("SECONDBOX_AGENT".into(), "1".into())
        );
    }

    #[test]
    fn validates_the_real_libkrun_builder_without_entering() {
        let root = tempdir().unwrap();
        let workspace = tempfile::NamedTempFile::new().unwrap();
        workspace.as_file().set_len(64 * MIB).unwrap();
        let result = LaunchConfiguration::from_start(&valid_start(root.path()))
            .unwrap()
            .build_with_workspace(workspace.path().to_owned(), None, None);
        if let Err(error) = result {
            panic!("libkrun rejected the launch configuration: {error}");
        }
    }

    #[test]
    fn rejects_fractional_memory_and_untranslated_policy() {
        let root = tempdir().unwrap();
        let mut start = valid_start(root.path());
        start.memory_bytes += 1;
        assert_eq!(
            LaunchConfiguration::from_start(&start),
            Err(LaunchError::Memory)
        );
        start.memory_bytes -= 1;
        start.network_policy = None;
        assert_eq!(
            LaunchConfiguration::from_start(&start),
            Err(LaunchError::Network)
        );
    }
}
