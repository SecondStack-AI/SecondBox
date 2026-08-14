//! Real-hypervisor inherited-descriptor, control-plane, and lifecycle proof.

use std::fs::{File, OpenOptions};
use std::os::fd::AsRawFd;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

use microsandbox::{ExecEvent, Sandbox, sandbox::SandboxStatus};
use microsandbox_image::ext4::{Ext4FormatOptions, format_ext4_file_with_uuid};
use microsandbox_types::DiskImageFormat;

const IMAGE_BYTES: u64 = 256 * 1024 * 1024;
const JOURNAL_BLOCKS: u32 = 4096;
const WORKSPACE_UUID: [u8; 16] = [0x63; 16];
const SHUTDOWN_DEADLINE: Duration = Duration::from_secs(45);
const MARKER: &str = "secondbox-task0l-marker";

/// Bounded evidence returned after a real Microsandbox VM exits cleanly.
#[derive(Debug, Eq, PartialEq)]
pub struct VmDescriptorLifecycleProof {
    /// Workspace inode held continuously by the probe.
    pub workspace_inode: u64,
    /// Local VMM process observed while the VM was running.
    pub vmm_pid: u32,
    /// Buffered agent command output.
    pub buffered_output: String,
    /// Streaming agent command output.
    pub streaming_output: String,
    /// Concurrent agent control-frame round-trip duration.
    pub ping_rtt_micros: u128,
    /// Graceful control-channel shutdown duration.
    pub shutdown_millis: u128,
    /// Marker recovered from the stopped raw-ext4 image.
    pub marker: String,
    /// VMM process governed by the inherited lifecycle pipe.
    pub lifecycle_pid: u32,
    /// Time from closing the lifecycle-pipe writer to stopped state.
    pub lifecycle_shutdown_millis: u128,
    /// Marker flushed by lifecycle-pipe shutdown.
    pub lifecycle_marker: String,
    /// VMM process used to exercise the explicit deadline force-kill path.
    pub force_kill_pid: u32,
    /// Time from explicit kill request to stopped state.
    pub force_kill_millis: u128,
}

/// Boot a real VM with a raw-ext4 image named only by an inherited descriptor, exercise the
/// agent and control channels concurrently, then prove bounded shutdown and disk persistence.
pub async fn run_vm_descriptor_lifecycle_proof(
    work_dir: &Path,
    rootfs: &Path,
) -> Result<VmDescriptorLifecycleProof, String> {
    if !rootfs.is_dir() {
        return Err(format!("rootfs is not a directory: {}", rootfs.display()));
    }
    std::fs::create_dir(work_dir)
        .map_err(|error| format!("create exclusive VM proof directory: {error}"))?;
    let original_path = work_dir.join("workspace.ext4");
    let durable_path = work_dir.join("workspace-after-attach.ext4");
    let mut workspace = OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(&original_path)
        .map_err(|error| format!("create workspace image: {error}"))?;
    format_ext4_file_with_uuid(
        &mut workspace,
        &Ext4FormatOptions {
            size_bytes: IMAGE_BYTES,
            journal_blocks: JOURNAL_BLOCKS,
        },
        WORKSPACE_UUID,
    )
    .map_err(|error| format!("format workspace descriptor: {error}"))?;
    workspace
        .sync_all()
        .map_err(|error| format!("flush formatted workspace: {error}"))?;
    clear_cloexec(&workspace)?;
    let before = workspace
        .metadata()
        .map_err(|error| format!("stat workspace descriptor: {error}"))?;
    let inherited_path = descriptor_path(workspace.as_raw_fd());

    let name = format!("secondbox-task0l-{}", std::process::id());
    if let Ok(existing) = Sandbox::get(&name).await {
        let _ = existing.kill().await;
        let _ = existing.remove().await;
    }

    let sandbox = Sandbox::builder(&name)
        .image(rootfs.to_path_buf())
        .cpus(1)
        .memory(384)
        .disable_metrics_sample()
        .volume("/workspace", |mount| {
            mount
                .disk(&inherited_path)
                .format(DiskImageFormat::Raw)
                .fstype("ext4")
        })
        .replace()
        .create()
        .await
        .map_err(|error| format!("create descriptor-backed sandbox: {error}"))?;

    let local = sandbox
        .local()
        .ok_or_else(|| "probe unexpectedly created a non-local sandbox".to_string())?;
    let process = local
        .handle
        .as_ref()
        .ok_or_else(|| "attached sandbox has no owned process handle".to_string())?;
    let vmm_pid = process.lock().await.pid();
    verify_inherited_inode(vmm_pid, workspace.as_raw_fd(), before.dev(), before.ino())?;

    std::fs::rename(&original_path, &durable_path)
        .map_err(|error| format!("rename attached workspace image: {error}"))?;
    if original_path.exists() {
        return Err("workspace's original pathname still exists after rename".to_string());
    }

    let mut stream = sandbox
        .shell_stream("printf stream-a; sleep 1; printf stream-b")
        .await
        .map_err(|error| format!("start streaming command: {error}"))?;
    let ping_started = Instant::now();
    let (ping, buffered, streamed) = tokio::join!(
        sandbox.ping(),
        sandbox
            .shell("printf buffered-ok; printf secondbox-task0l-marker > /workspace/marker; sync"),
        collect_stream(&mut stream),
    );
    let ping = ping.map_err(|error| format!("concurrent control ping: {error}"))?;
    let buffered = buffered.map_err(|error| format!("buffered agent command: {error}"))?;
    if !buffered.status().success {
        return Err(format!(
            "buffered agent command exited {}",
            buffered.status().code
        ));
    }
    let buffered_output = buffered
        .stdout()
        .map_err(|error| format!("decode buffered output: {error}"))?;
    if buffered_output != "buffered-ok" {
        return Err(format!("unexpected buffered output: {buffered_output:?}"));
    }
    let streaming_output = streamed?;
    if streaming_output != "stream-astream-b" {
        return Err(format!("unexpected streaming output: {streaming_output:?}"));
    }

    let shutdown_started = Instant::now();
    let exit = tokio::time::timeout(SHUTDOWN_DEADLINE, sandbox.stop_and_wait())
        .await
        .map_err(|_| format!("control-channel shutdown exceeded {SHUTDOWN_DEADLINE:?}"))?
        .map_err(|error| format!("control-channel shutdown: {error}"))?;
    if !exit.success() {
        return Err(format!(
            "VMM exited unsuccessfully after graceful shutdown: {exit}"
        ));
    }
    let shutdown_millis = shutdown_started.elapsed().as_millis();

    let after = workspace
        .metadata()
        .map_err(|error| format!("stat parent-held descriptor after shutdown: {error}"))?;
    let named = std::fs::metadata(&durable_path)
        .map_err(|error| format!("stat renamed workspace after shutdown: {error}"))?;
    if (before.dev(), before.ino()) != (after.dev(), after.ino())
        || (after.dev(), after.ino()) != (named.dev(), named.ino())
    {
        return Err("VMM replaced the caller-owned workspace inode".to_string());
    }
    verify_e2fsck(&durable_path)?;
    let marker = read_marker(&durable_path)?;
    if marker != MARKER {
        return Err(format!(
            "persisted marker is {marker:?}, expected {MARKER:?}"
        ));
    }

    let handle = Sandbox::get(&name)
        .await
        .map_err(|error| format!("get stopped sandbox for cleanup: {error}"))?;
    handle
        .remove()
        .await
        .map_err(|error| format!("remove stopped sandbox record: {error}"))?;

    let lifecycle = run_lifecycle_eof_proof(work_dir, rootfs).await?;
    let force_kill = run_force_kill_proof(work_dir, rootfs).await?;

    Ok(VmDescriptorLifecycleProof {
        workspace_inode: before.ino(),
        vmm_pid,
        buffered_output,
        streaming_output,
        ping_rtt_micros: ping
            .latency
            .as_micros()
            .max(ping_started.elapsed().as_micros().min(1)),
        shutdown_millis,
        marker,
        lifecycle_pid: lifecycle.pid,
        lifecycle_shutdown_millis: lifecycle.shutdown_millis,
        lifecycle_marker: lifecycle.marker,
        force_kill_pid: force_kill.pid,
        force_kill_millis: force_kill.shutdown_millis,
    })
}

struct LifecycleEofProof {
    pid: u32,
    shutdown_millis: u128,
    marker: String,
}

async fn run_lifecycle_eof_proof(
    work_dir: &Path,
    rootfs: &Path,
) -> Result<LifecycleEofProof, String> {
    let lifecycle_dir = work_dir.join("lifecycle-eof");
    std::fs::create_dir(&lifecycle_dir)
        .map_err(|error| format!("create lifecycle proof directory: {error}"))?;
    let image_path = lifecycle_dir.join("workspace.ext4");
    let mut image = OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(&image_path)
        .map_err(|error| format!("create lifecycle workspace: {error}"))?;
    format_ext4_file_with_uuid(
        &mut image,
        &Ext4FormatOptions {
            size_bytes: IMAGE_BYTES,
            journal_blocks: JOURNAL_BLOCKS,
        },
        [0x74; 16],
    )
    .map_err(|error| format!("format lifecycle workspace: {error}"))?;
    image
        .sync_all()
        .map_err(|error| format!("flush lifecycle workspace: {error}"))?;
    clear_cloexec(&image)?;
    let metadata = image
        .metadata()
        .map_err(|error| format!("stat lifecycle workspace: {error}"))?;
    let descriptor = descriptor_path(image.as_raw_fd());
    let name = format!("secondbox-task0l-eof-{}", std::process::id());

    let sandbox = Sandbox::builder(&name)
        .image(rootfs.to_path_buf())
        .cpus(1)
        .memory(384)
        .disable_metrics_sample()
        .volume("/workspace", |mount| {
            mount
                .disk(&descriptor)
                .format(DiskImageFormat::Raw)
                .fstype("ext4")
        })
        .replace()
        .create()
        .await
        .map_err(|error| format!("create lifecycle-pipe sandbox: {error}"))?;
    let process = sandbox
        .local()
        .and_then(|local| local.handle.as_ref())
        .ok_or_else(|| "lifecycle sandbox has no owned process handle".to_string())?;
    let pid = process.lock().await.pid();
    verify_inherited_inode(pid, image.as_raw_fd(), metadata.dev(), metadata.ino())?;
    let output = sandbox
        .shell("printf lifecycle-eof-marker > /workspace/marker; sync")
        .await
        .map_err(|error| format!("write lifecycle marker: {error}"))?;
    if !output.status().success {
        return Err(format!(
            "lifecycle marker command exited {}",
            output.status().code
        ));
    }

    let started = Instant::now();
    drop(sandbox);
    let deadline = started + SHUTDOWN_DEADLINE;
    let stopped = loop {
        match Sandbox::get(&name).await {
            Ok(handle) if handle.status_snapshot() == SandboxStatus::Stopped => break handle,
            Ok(handle) if Instant::now() >= deadline => {
                handle
                    .kill()
                    .await
                    .map_err(|error| format!("force-kill lifecycle timeout: {error}"))?;
                return Err(format!(
                    "lifecycle-pipe EOF exceeded {SHUTDOWN_DEADLINE:?}; force-killed"
                ));
            }
            Ok(_) => tokio::time::sleep(Duration::from_millis(100)).await,
            Err(error) => return Err(format!("observe lifecycle sandbox after EOF: {error}")),
        }
    };
    let shutdown_millis = started.elapsed().as_millis();
    verify_e2fsck(&image_path)?;
    let marker = read_marker(&image_path)?;
    if marker != "lifecycle-eof-marker" {
        return Err(format!("lifecycle marker is {marker:?}"));
    }
    stopped
        .remove()
        .await
        .map_err(|error| format!("remove lifecycle sandbox: {error}"))?;
    Ok(LifecycleEofProof {
        pid,
        shutdown_millis,
        marker,
    })
}

async fn run_force_kill_proof(work_dir: &Path, rootfs: &Path) -> Result<LifecycleEofProof, String> {
    let force_dir = work_dir.join("force-kill");
    std::fs::create_dir(&force_dir)
        .map_err(|error| format!("create force-kill proof directory: {error}"))?;
    let name = format!("secondbox-task8m-kill-{}", std::process::id());
    let sandbox = Sandbox::builder(&name)
        .image(rootfs.to_path_buf())
        .cpus(1)
        .memory(384)
        .disable_metrics_sample()
        .replace()
        .create()
        .await
        .map_err(|error| format!("create force-kill sandbox: {error}"))?;
    let process = sandbox
        .local()
        .and_then(|local| local.handle.as_ref())
        .ok_or_else(|| "force-kill sandbox has no owned process handle".to_string())?;
    let pid = process.lock().await.pid();
    let started = Instant::now();
    tokio::time::timeout(SHUTDOWN_DEADLINE, sandbox.kill())
        .await
        .map_err(|_| format!("force-kill exceeded {SHUTDOWN_DEADLINE:?}"))?
        .map_err(|error| format!("force-kill sandbox: {error}"))?;
    let stopped = Sandbox::get(&name)
        .await
        .map_err(|error| format!("get force-killed sandbox: {error}"))?;
    if stopped.status_snapshot() != SandboxStatus::Stopped {
        return Err("force-killed sandbox did not reach stopped state".to_string());
    }
    stopped
        .remove()
        .await
        .map_err(|error| format!("remove force-killed sandbox: {error}"))?;
    Ok(LifecycleEofProof {
        pid,
        shutdown_millis: started.elapsed().as_millis(),
        marker: "not-applicable-force-kill".to_string(),
    })
}

async fn collect_stream(stream: &mut microsandbox::ExecHandle) -> Result<String, String> {
    let mut output = Vec::new();
    while let Some(event) = stream.recv().await {
        match event {
            ExecEvent::Stdout(bytes) => output.extend_from_slice(&bytes),
            ExecEvent::Exited { code: 0 } => break,
            ExecEvent::Exited { code } => return Err(format!("streaming command exited {code}")),
            ExecEvent::Failed(error) => return Err(format!("streaming command failed: {error:?}")),
            _ => {}
        }
    }
    String::from_utf8(output).map_err(|error| format!("decode streaming output: {error}"))
}

fn clear_cloexec(file: &File) -> Result<(), String> {
    let fd = file.as_raw_fd();
    // SAFETY: fd is owned by `file` for the duration of both fcntl calls.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
    if flags < 0 {
        return Err(format!(
            "read workspace descriptor flags: {}",
            std::io::Error::last_os_error()
        ));
    }
    // SAFETY: this changes only the close-on-exec bit on the same valid descriptor.
    if unsafe { libc::fcntl(fd, libc::F_SETFD, flags & !libc::FD_CLOEXEC) } < 0 {
        return Err(format!(
            "clear workspace close-on-exec: {}",
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

fn descriptor_path(fd: i32) -> PathBuf {
    #[cfg(target_os = "macos")]
    {
        return PathBuf::from(format!("/dev/fd/{fd}"));
    }
    #[cfg(not(target_os = "macos"))]
    {
        PathBuf::from(format!("/proc/self/fd/{fd}"))
    }
}

#[cfg(target_os = "linux")]
fn verify_inherited_inode(pid: u32, fd: i32, dev: u64, ino: u64) -> Result<(), String> {
    let path = PathBuf::from(format!("/proc/{pid}/fd/{fd}"));
    let metadata = std::fs::metadata(&path)
        .map_err(|error| format!("stat inherited VMM descriptor {}: {error}", path.display()))?;
    if (metadata.dev(), metadata.ino()) != (dev, ino) {
        return Err("VMM inherited descriptor points at a different inode".to_string());
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn verify_inherited_inode(pid: u32, fd: i32, dev: u64, ino: u64) -> Result<(), String> {
    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct ProcFileInfo {
        open_flags: u32,
        status: u32,
        offset: i64,
        file_type: i32,
        guard_flags: u32,
    }
    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct VInfoStat {
        dev: u32,
        mode: u16,
        nlink: u16,
        ino: u64,
        uid: u32,
        gid: u32,
        times_and_size: [i64; 9],
        blocks: i64,
        block_size: i32,
        flags: u32,
        generation: u32,
        rdev: u32,
        spare: [i64; 2],
    }
    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct VnodeInfo {
        stat: VInfoStat,
        vnode_type: i32,
        pad: i32,
        fsid: [i32; 2],
    }
    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct VnodeFdInfo {
        file: ProcFileInfo,
        vnode: VnodeInfo,
    }
    unsafe extern "C" {
        fn proc_pidfdinfo(
            pid: libc::c_int,
            fd: libc::c_int,
            flavor: libc::c_int,
            buffer: *mut libc::c_void,
            buffer_size: libc::c_int,
        ) -> libc::c_int;
    }
    const PROC_PIDFDVNODEINFO: libc::c_int = 1;
    let mut info = VnodeFdInfo::default();
    // SAFETY: `info` is a writable C-layout buffer of the exact advertised size and remains alive
    // for the call. pid/fd identify the child process and inherited descriptor under test.
    let read = unsafe {
        proc_pidfdinfo(
            pid as libc::c_int,
            fd,
            PROC_PIDFDVNODEINFO,
            (&mut info as *mut VnodeFdInfo).cast(),
            std::mem::size_of::<VnodeFdInfo>() as libc::c_int,
        )
    };
    if read != std::mem::size_of::<VnodeFdInfo>() as libc::c_int {
        return Err(format!(
            "inspect inherited VMM descriptor with proc_pidfdinfo: read={read} error={}",
            std::io::Error::last_os_error()
        ));
    }
    if (u64::from(info.vnode.stat.dev), info.vnode.stat.ino) != (dev, ino) {
        return Err("VMM inherited descriptor points at a different inode".to_string());
    }
    Ok(())
}

fn verify_e2fsck(path: &Path) -> Result<(), String> {
    let output = Command::new("e2fsck")
        .args(["-fn"])
        .arg(path)
        .output()
        .map_err(|error| format!("execute e2fsck: {error}"))?;
    if output.status.success() {
        return Ok(());
    }
    Err(format!(
        "e2fsck rejected stopped image: {}",
        bounded(&output.stderr)
    ))
}

fn read_marker(path: &Path) -> Result<String, String> {
    let output = Command::new("debugfs")
        .args(["-R", "cat /marker"])
        .arg(path)
        .output()
        .map_err(|error| format!("execute debugfs: {error}"))?;
    if !output.status.success() {
        return Err(format!(
            "debugfs could not read marker: {}",
            bounded(&output.stderr)
        ));
    }
    String::from_utf8(output.stdout)
        .map(|value| value.trim().to_string())
        .map_err(|error| format!("decode marker: {error}"))
}

fn bounded(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes).chars().take(2048).collect()
}
