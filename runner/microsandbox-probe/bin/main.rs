use std::env;
use std::path::PathBuf;
use std::process::ExitCode;

use secondbox_microsandbox_probe::ext4::run_descriptor_uuid_proof;
use secondbox_microsandbox_probe::network::run_network_policy_proof;
use secondbox_microsandbox_probe::vm::run_vm_descriptor_lifecycle_proof;

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("SecondBox Microsandbox probe: {error}");
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), String> {
    let mut args = env::args_os();
    let _program = args.next();
    let command = args
        .next()
        .and_then(|value| value.into_string().ok())
        .ok_or_else(usage)?;
    if command == "vm-descriptor-lifecycle" {
        return run_vm(args).await;
    }
    if command == "network-policy" {
        return run_network(args).await;
    }
    if command != "ext4-descriptor-uuid" {
        return Err(usage());
    }
    let flag = args
        .next()
        .and_then(|value| value.into_string().ok())
        .ok_or_else(usage)?;
    if flag != "--work-dir" {
        return Err(usage());
    }
    let work_dir = args.next().map(PathBuf::from).ok_or_else(usage)?;
    if args.next().is_some() {
        return Err(usage());
    }

    let result = run_descriptor_uuid_proof(&work_dir)?;
    println!(
        "proof=ext4-descriptor-uuid source_inode={} clone_inode={} logical_bytes={} source_allocated_bytes={} clone_allocated_bytes={} source_uuid={} clone_uuid={} status=passed",
        result.source_inode,
        result.clone_inode,
        result.logical_bytes,
        result.source_allocated_bytes,
        result.clone_allocated_bytes,
        encode_hex(&result.source_uuid),
        encode_hex(&result.clone_uuid),
    );
    Ok(())
}

async fn run_network(mut args: impl Iterator<Item = std::ffi::OsString>) -> Result<(), String> {
    let work_flag = args
        .next()
        .and_then(|v| v.into_string().ok())
        .ok_or_else(usage)?;
    let work_dir = args.next().map(PathBuf::from).ok_or_else(usage)?;
    let root_flag = args
        .next()
        .and_then(|v| v.into_string().ok())
        .ok_or_else(usage)?;
    let rootfs = args.next().map(PathBuf::from).ok_or_else(usage)?;
    if work_flag != "--work-dir" || root_flag != "--rootfs" || args.next().is_some() {
        return Err(usage());
    }
    let proof = run_network_policy_proof(&work_dir, &rootfs).await?;
    println!(
        "proof=network-policy allowed_bytes={} denied_domain={} denied_private={} denied_metadata={} deny_all={} dns_change={} status=passed",
        proof.allowed_bytes,
        proof.denied_domain,
        proof.denied_private,
        proof.denied_metadata,
        proof.deny_all,
        proof.dns_change,
    );
    Ok(())
}

async fn run_vm(mut args: impl Iterator<Item = std::ffi::OsString>) -> Result<(), String> {
    let work_flag = args
        .next()
        .and_then(|v| v.into_string().ok())
        .ok_or_else(usage)?;
    let work_dir = args.next().map(PathBuf::from).ok_or_else(usage)?;
    let root_flag = args
        .next()
        .and_then(|v| v.into_string().ok())
        .ok_or_else(usage)?;
    let rootfs = args.next().map(PathBuf::from).ok_or_else(usage)?;
    if work_flag != "--work-dir" || root_flag != "--rootfs" || args.next().is_some() {
        return Err(usage());
    }
    let proof = run_vm_descriptor_lifecycle_proof(&work_dir, &rootfs).await?;
    println!(
        "proof=vm-descriptor-lifecycle inode={} vmm_pid={} buffered={} streamed={} ping_rtt_micros={} shutdown_millis={} marker={} lifecycle_pid={} lifecycle_shutdown_millis={} lifecycle_marker={} force_kill_pid={} force_kill_millis={} status=passed",
        proof.workspace_inode,
        proof.vmm_pid,
        proof.buffered_output,
        proof.streaming_output,
        proof.ping_rtt_micros,
        proof.shutdown_millis,
        proof.marker,
        proof.lifecycle_pid,
        proof.lifecycle_shutdown_millis,
        proof.lifecycle_marker,
        proof.force_kill_pid,
        proof.force_kill_millis,
    );
    Ok(())
}

fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        encoded.push(HEX[(byte >> 4) as usize] as char);
        encoded.push(HEX[(byte & 0x0f) as usize] as char);
    }
    encoded
}

fn usage() -> String {
    "usage: secondbox-microsandbox-probe <ext4-descriptor-uuid --work-dir <new-empty-directory> | vm-descriptor-lifecycle|network-policy --work-dir <new-empty-directory> --rootfs <directory>>"
        .to_string()
}
