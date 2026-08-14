//! Exact translation from the private helper start request to libkrun.

use std::{
    ffi::{CStr, CString},
    fs, io,
    os::unix::{ffi::OsStrExt, fs::MetadataExt},
    path::{Path, PathBuf},
};

use microsandbox_network::{
    builder::NetworkBuilder,
    network::SmoltcpNetwork,
    policy::{Action, Destination, Direction, NetworkPolicy, PortRange, Protocol, Rule},
};
use microsandbox_types::DeploymentProfile;
use msb_krun::{DiskImageFormat, SyncMode, VmBuilder};
use thiserror::Error;

use crate::protocol::{
    HelperNetworkDestination, HelperNetworkPolicyMode, HelperNetworkProtocol, StartRequest,
    helper_network_destination::Target,
};

pub const WORKSPACE_DESCRIPTOR_PATH: &str = crate::fd::WORKSPACE_DESCRIPTOR_PATH;
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
    pub network_slot: u64,
}

pub struct RunningVm {
    vm: msb_krun::Vm,
    _network_runtime: tokio::runtime::Runtime,
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

#[derive(Debug, Error)]
pub enum BuildError {
    #[error("SecondBox Microsandbox helper network policy cannot be represented: {0}")]
    NetworkPolicy(String),
    #[error("SecondBox Microsandbox helper network configuration: {0}")]
    NetworkConfiguration(String),
    #[error("SecondBox Microsandbox helper network runtime: {0}")]
    NetworkRuntime(#[from] std::io::Error),
    #[error("SecondBox Microsandbox helper ephemeral root materialization: {0}")]
    EphemeralRoot(String),
    #[error("SecondBox Microsandbox helper libkrun configuration: {0}")]
    Libkrun(#[from] msb_krun::Error),
}

impl RunningVm {
    pub fn exit_handle(&self) -> msb_krun::ExitHandle {
        self.vm.exit_handle()
    }

    pub fn enter(self) -> Result<std::convert::Infallible, msb_krun::Error> {
        let Self {
            vm,
            _network_runtime,
        } = self;
        vm.enter()
    }
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
            network_slot: u64::from_be_bytes(
                start.workspace_uuid[..8]
                    .try_into()
                    .map_err(|_| LaunchError::Workspace)?,
            ) % 65_536,
        })
    }

    /// Build the complete libkrun configuration without starting its non-returning VMM loop.
    pub fn build(
        self,
        console: crate::console::AgentConsoleBackend,
        runtime_dir: &std::path::Path,
    ) -> Result<RunningVm, BuildError> {
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
    ) -> Result<RunningVm, BuildError> {
        let mut root = self.root;
        let libkrunfw = self.libkrunfw;
        let agentd = self.agentd;
        let vcpus = self.vcpus;
        let memory_mib = self.memory_mib;
        let mut environment = self.environment;
        let policy = microsandbox_policy(&self.network)?;
        let network_config = NetworkBuilder::new()
            .enabled(true)
            .policy(policy)
            .build()
            .map_err(|error| BuildError::NetworkConfiguration(error.to_string()))?;
        let network_runtime = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .thread_name("secondbox-microsandbox-network")
            .build()?;
        let mut network = SmoltcpNetwork::new_with_profile(
            network_config,
            self.network_slot,
            DeploymentProfile::MultiTenant,
        )
        .map_err(|error| BuildError::NetworkConfiguration(error.to_string()))?;
        network.start(network_runtime.handle().clone());
        let guest_mac = network.guest_mac();
        let net_backend = network.take_backend();
        environment.extend(network.guest_env_vars());
        if let Some(runtime_dir) = runtime_dir.as_deref() {
            let ephemeral_root = runtime_dir.join("rootfs");
            clone_flat_root(&root, &ephemeral_root)?;
            root = ephemeral_root;
        }
        let mut builder = VmBuilder::new()
            .machine(|machine| {
                let machine = machine
                    .vcpus(vcpus)
                    .memory_mib(memory_mib)
                    .max_vcpus(vcpus)
                    .max_memory_mib(memory_mib)
                    .enable_inet_hijack(false);
                #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
                {
                    machine.split_irqchip(true)
                }
                #[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
                {
                    machine
                }
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
            })
            .net(move |net| net.mac(guest_mac).custom(net_backend));
        if let Some(runtime_dir) = runtime_dir {
            builder = builder.fs(|filesystem| filesystem.tag("msb_runtime").path(runtime_dir));
        }
        if let Some(console) = console {
            builder = builder.console(|ports| {
                ports
                    .output(crate::fd::AGENT_CONSOLE_DESCRIPTOR_PATH)
                    .custom("agent", Box::new(console))
            });
        }
        Ok(RunningVm {
            vm: builder.build()?,
            _network_runtime: network_runtime,
        })
    }
}

// A directory-backed libkrun root is writable by the guest. Never expose the
// immutable shared flat-root materialization directly: every helper gets one
// private ephemeral tree, while durable user data remains exclusively on the
// separately attached Workspace image.
fn clone_flat_root(source: &Path, destination: &Path) -> Result<(), BuildError> {
    fs::create_dir(destination).map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
    clone_flat_root_entries(source, destination)?;
    let metadata = fs::symlink_metadata(source)
        .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
    copy_flat_root_metadata(source, destination, &metadata)
}

fn clone_flat_root_entries(source: &Path, destination: &Path) -> Result<(), BuildError> {
    for entry in
        fs::read_dir(source).map_err(|error| BuildError::EphemeralRoot(error.to_string()))?
    {
        let entry = entry.map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
        let metadata = fs::symlink_metadata(entry.path())
            .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
        let target = destination.join(entry.file_name());
        if metadata.is_dir() {
            fs::create_dir(&target)
                .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
            clone_flat_root_entries(&entry.path(), &target)?;
        } else if metadata.is_file() {
            fs::copy(entry.path(), &target)
                .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
        } else if metadata.file_type().is_symlink() {
            let link = fs::read_link(entry.path())
                .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
            std::os::unix::fs::symlink(link, &target)
                .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
        } else {
            return Err(BuildError::EphemeralRoot(format!(
                "unsupported flat-root entry type: {}",
                entry.path().display()
            )));
        }
        copy_flat_root_metadata(&entry.path(), &target, &metadata)?;
    }
    Ok(())
}

fn copy_flat_root_metadata(
    source: &Path,
    destination: &Path,
    metadata: &fs::Metadata,
) -> Result<(), BuildError> {
    let destination_c = path_c_string(destination)?;
    let owner_result = unsafe {
        // The path is a validated NUL-free CString and lchown intentionally does
        // not follow a flat-root symlink.
        libc::lchown(destination_c.as_ptr(), metadata.uid(), metadata.gid())
    };
    if owner_result != 0 {
        return Err(ephemeral_io_error("preserve owner", destination));
    }
    if !metadata.file_type().is_symlink() {
        fs::set_permissions(destination, metadata.permissions())
            .map_err(|error| BuildError::EphemeralRoot(error.to_string()))?;
    }
    copy_xattrs(source, destination)?;
    let times = [
        libc::timespec {
            tv_sec: metadata.atime(),
            tv_nsec: metadata.atime_nsec(),
        },
        libc::timespec {
            tv_sec: metadata.mtime(),
            tv_nsec: metadata.mtime_nsec(),
        },
    ];
    let time_result = unsafe {
        // Both pointers remain valid for the duration of this call. NOFOLLOW is
        // required so symlink metadata is preserved without touching its target.
        libc::utimensat(
            libc::AT_FDCWD,
            destination_c.as_ptr(),
            times.as_ptr(),
            libc::AT_SYMLINK_NOFOLLOW,
        )
    };
    if time_result != 0 {
        return Err(ephemeral_io_error("preserve timestamps", destination));
    }
    Ok(())
}

fn copy_xattrs(source: &Path, destination: &Path) -> Result<(), BuildError> {
    let source_c = path_c_string(source)?;
    let destination_c = path_c_string(destination)?;
    let names = list_xattrs(&source_c)
        .map_err(|error| BuildError::EphemeralRoot(format!("list xattrs: {error}")))?;
    for name_bytes in names
        .split(|byte| *byte == 0)
        .filter(|name| !name.is_empty())
    {
        let name = CString::new(name_bytes).map_err(|_| {
            BuildError::EphemeralRoot("flat-root xattr name contains NUL".to_owned())
        })?;
        let value = get_xattr(&source_c, &name)
            .map_err(|error| BuildError::EphemeralRoot(format!("read xattr: {error}")))?;
        set_xattr(&destination_c, &name, &value)
            .map_err(|error| BuildError::EphemeralRoot(format!("write xattr: {error}")))?;
    }
    Ok(())
}

fn path_c_string(path: &Path) -> Result<CString, BuildError> {
    CString::new(path.as_os_str().as_bytes())
        .map_err(|_| BuildError::EphemeralRoot(format!("path contains NUL: {}", path.display())))
}

fn ephemeral_io_error(operation: &str, path: &Path) -> BuildError {
    BuildError::EphemeralRoot(format!(
        "{operation} {}: {}",
        path.display(),
        io::Error::last_os_error()
    ))
}

#[cfg(target_os = "linux")]
fn list_xattrs(path: &CStr) -> io::Result<Vec<u8>> {
    let size = unsafe { libc::llistxattr(path.as_ptr(), std::ptr::null_mut(), 0) };
    if size < 0 {
        return Err(io::Error::last_os_error());
    }
    let mut result = vec![0_u8; size as usize];
    if result.is_empty() {
        return Ok(result);
    }
    let read = unsafe {
        libc::llistxattr(
            path.as_ptr(),
            result.as_mut_ptr().cast::<libc::c_char>(),
            result.len(),
        )
    };
    if read < 0 {
        return Err(io::Error::last_os_error());
    }
    result.truncate(read as usize);
    Ok(result)
}

#[cfg(target_os = "macos")]
fn list_xattrs(path: &CStr) -> io::Result<Vec<u8>> {
    let size =
        unsafe { libc::listxattr(path.as_ptr(), std::ptr::null_mut(), 0, libc::XATTR_NOFOLLOW) };
    if size < 0 {
        return Err(io::Error::last_os_error());
    }
    let mut result = vec![0_u8; size as usize];
    if result.is_empty() {
        return Ok(result);
    }
    let read = unsafe {
        libc::listxattr(
            path.as_ptr(),
            result.as_mut_ptr().cast::<libc::c_char>(),
            result.len(),
            libc::XATTR_NOFOLLOW,
        )
    };
    if read < 0 {
        return Err(io::Error::last_os_error());
    }
    result.truncate(read as usize);
    Ok(result)
}

#[cfg(target_os = "linux")]
fn get_xattr(path: &CStr, name: &CStr) -> io::Result<Vec<u8>> {
    let size = unsafe { libc::lgetxattr(path.as_ptr(), name.as_ptr(), std::ptr::null_mut(), 0) };
    if size < 0 {
        return Err(io::Error::last_os_error());
    }
    let mut result = vec![0_u8; size as usize];
    let read = unsafe {
        libc::lgetxattr(
            path.as_ptr(),
            name.as_ptr(),
            result.as_mut_ptr().cast::<libc::c_void>(),
            result.len(),
        )
    };
    if read < 0 {
        return Err(io::Error::last_os_error());
    }
    result.truncate(read as usize);
    Ok(result)
}

#[cfg(target_os = "macos")]
fn get_xattr(path: &CStr, name: &CStr) -> io::Result<Vec<u8>> {
    let size = unsafe {
        libc::getxattr(
            path.as_ptr(),
            name.as_ptr(),
            std::ptr::null_mut(),
            0,
            0,
            libc::XATTR_NOFOLLOW,
        )
    };
    if size < 0 {
        return Err(io::Error::last_os_error());
    }
    let mut result = vec![0_u8; size as usize];
    let read = unsafe {
        libc::getxattr(
            path.as_ptr(),
            name.as_ptr(),
            result.as_mut_ptr().cast::<libc::c_void>(),
            result.len(),
            0,
            libc::XATTR_NOFOLLOW,
        )
    };
    if read < 0 {
        return Err(io::Error::last_os_error());
    }
    result.truncate(read as usize);
    Ok(result)
}

#[cfg(target_os = "linux")]
fn set_xattr(path: &CStr, name: &CStr, value: &[u8]) -> io::Result<()> {
    let result = unsafe {
        libc::lsetxattr(
            path.as_ptr(),
            name.as_ptr(),
            value.as_ptr().cast::<libc::c_void>(),
            value.len(),
            0,
        )
    };
    if result != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn set_xattr(path: &CStr, name: &CStr, value: &[u8]) -> io::Result<()> {
    let result = unsafe {
        libc::setxattr(
            path.as_ptr(),
            name.as_ptr(),
            value.as_ptr().cast::<libc::c_void>(),
            value.len(),
            0,
            libc::XATTR_NOFOLLOW,
        )
    };
    if result != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn microsandbox_policy(translated: &TranslatedNetworkPolicy) -> Result<NetworkPolicy, BuildError> {
    let TranslatedNetworkPolicy::AllowList(destinations) = translated else {
        return Ok(NetworkPolicy::none());
    };
    let mut rules = Vec::with_capacity(destinations.len() + 1);
    if destinations.iter().any(|destination| destination.is_domain) {
        rules.push(Rule::allow_dns());
    }
    for destination in destinations {
        let target = if destination.is_domain {
            Destination::Domain(destination.target.parse().map_err(|error| {
                BuildError::NetworkPolicy(format!("invalid domain {}: {error}", destination.target))
            })?)
        } else {
            Destination::Cidr(destination.target.parse().map_err(|error| {
                BuildError::NetworkPolicy(format!("invalid CIDR {}: {error}", destination.target))
            })?)
        };
        rules.push(Rule {
            direction: Direction::Egress,
            destination: target,
            protocols: vec![Protocol::Tcp],
            ports: vec![PortRange::single(destination.port)],
            action: Action::Allow,
        });
    }
    Ok(NetworkPolicy {
        default_egress: Action::Deny,
        default_ingress: Action::Deny,
        rules,
    })
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
        HelperNetworkPolicyMode::AllowList => policy
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
        let test_asset = std::env::current_exe()
            .expect("resolve current test executable")
            .display()
            .to_string();
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
            libkrunfw_path: test_asset.clone(),
            agentd_path: test_asset,
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
    fn ephemeral_root_is_private_and_preserves_metadata() {
        let source = tempdir().unwrap();
        let destination = tempdir().unwrap();
        fs::create_dir(source.path().join("etc")).unwrap();
        fs::write(source.path().join("etc/hosts"), "immutable\n").unwrap();
        let xattr_name = CString::new("user.secondbox.clone-test").unwrap();
        let source_hosts = path_c_string(&source.path().join("etc/hosts")).unwrap();
        set_xattr(&source_hosts, &xattr_name, b"preserved").unwrap();
        let executable = source.path().join("init");
        fs::write(&executable, "agent").unwrap();
        let mut permissions = fs::metadata(&executable).unwrap().permissions();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            permissions.set_mode(0o755);
        }
        fs::set_permissions(&executable, permissions).unwrap();
        #[cfg(unix)]
        std::os::unix::fs::symlink("init", source.path().join("agentd")).unwrap();

        let clone = destination.path().join("rootfs");
        clone_flat_root(source.path(), &clone).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::{MetadataExt, PermissionsExt};
            assert_eq!(
                fs::metadata(clone.join("init"))
                    .unwrap()
                    .permissions()
                    .mode()
                    & 0o777,
                0o755
            );
            assert_eq!(
                fs::read_link(clone.join("agentd")).unwrap(),
                PathBuf::from("init")
            );
            assert_ne!(
                fs::metadata(source.path().join("etc/hosts")).unwrap().ino(),
                fs::metadata(clone.join("etc/hosts")).unwrap().ino()
            );
            let clone_hosts = path_c_string(&clone.join("etc/hosts")).unwrap();
            assert_eq!(get_xattr(&clone_hosts, &xattr_name).unwrap(), b"preserved");
            let source_metadata = fs::symlink_metadata(source.path().join("etc/hosts")).unwrap();
            let clone_metadata = fs::symlink_metadata(clone.join("etc/hosts")).unwrap();
            assert_eq!(clone_metadata.uid(), source_metadata.uid());
            assert_eq!(clone_metadata.gid(), source_metadata.gid());
            assert_eq!(clone_metadata.mtime(), source_metadata.mtime());
            assert_eq!(clone_metadata.mtime_nsec(), source_metadata.mtime_nsec());
        }
        fs::write(clone.join("etc/hosts"), "guest-private\n").unwrap();
        assert_eq!(
            fs::read_to_string(source.path().join("etc/hosts")).unwrap(),
            "immutable\n"
        );
        assert_eq!(
            fs::read_to_string(clone.join("etc/hosts")).unwrap(),
            "guest-private\n"
        );
    }

    #[test]
    fn translates_exact_network_rules_into_fail_closed_smoltcp_policy() {
        let translated = TranslatedNetworkPolicy::AllowList(vec![
            TranslatedNetworkDestination {
                target: "api.example.com".into(),
                is_domain: true,
                protocol: HelperNetworkProtocol::Https,
                port: 443,
            },
            TranslatedNetworkDestination {
                target: "93.184.216.0/24".into(),
                is_domain: false,
                protocol: HelperNetworkProtocol::Http,
                port: 8080,
            },
            TranslatedNetworkDestination {
                target: "2001:4860:4860::/48".into(),
                is_domain: false,
                protocol: HelperNetworkProtocol::Tcp,
                port: 8443,
            },
        ]);
        let policy = microsandbox_policy(&translated).unwrap();
        assert!(matches!(policy.default_egress, Action::Deny));
        assert!(matches!(policy.default_ingress, Action::Deny));
        assert_eq!(policy.rules.len(), 4);
        assert!(matches!(policy.rules[0].destination, Destination::Group(_)));
        assert!(matches!(
            policy.rules[1].destination,
            Destination::Domain(_)
        ));
        assert!(matches!(policy.rules[2].destination, Destination::Cidr(_)));
        assert!(matches!(policy.rules[3].destination, Destination::Cidr(_)));
        for (rule, port) in policy.rules[1..].iter().zip([443, 8080, 8443]) {
            assert_eq!(rule.protocols, vec![Protocol::Tcp]);
            assert_eq!(rule.ports[0].start, port);
            assert_eq!(rule.ports[0].end, port);
            assert!(matches!(rule.action, Action::Allow));
        }
        assert!(
            microsandbox_policy(&TranslatedNetworkPolicy::DenyAll)
                .unwrap()
                .rules
                .is_empty()
        );
        assert!(
            microsandbox_policy(&TranslatedNetworkPolicy::AllowList(Vec::new()))
                .unwrap()
                .rules
                .is_empty()
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
