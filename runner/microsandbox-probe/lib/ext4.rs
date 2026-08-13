//! Raw-ext4 inherited-descriptor and clone-UUID feasibility proof.

use std::fs::{File, OpenOptions};
use std::io::{Read, Seek, SeekFrom};
use std::os::fd::AsRawFd;
use std::os::unix::fs::MetadataExt;
use std::path::Path;
use std::process::Command;

use microsandbox_image::ext4::{Ext4FormatOptions, format_ext4_file_with_uuid, rewrite_uuid};

//--------------------------------------------------------------------------------------------------
// Constants
//--------------------------------------------------------------------------------------------------

const EXT4_IMAGE_BYTES: u64 = 256 * 1024 * 1024;
const EXT4_JOURNAL_BLOCKS: u32 = 4096;
const EXT4_SUPERBLOCK_OFFSET: u64 = 1024;
const EXT4_UUID_OFFSET: u64 = EXT4_SUPERBLOCK_OFFSET + 0x68;
const FICLONE: libc::c_ulong = 0x4004_9409;
const SOURCE_UUID: [u8; 16] = [0x41; 16];
const CLONE_UUID: [u8; 16] = [0x52; 16];

//--------------------------------------------------------------------------------------------------
// Types
//--------------------------------------------------------------------------------------------------

/// Evidence returned by the descriptor and UUID proof.
#[derive(Debug, Eq, PartialEq)]
pub struct DescriptorUuidProof {
    /// Inode held by the original parent descriptor before and after formatting.
    pub source_inode: u64,
    /// Inode allocated for the reflink clone.
    pub clone_inode: u64,
    /// Sparse logical image size in bytes.
    pub logical_bytes: u64,
    /// UUID retained by the source image.
    pub source_uuid: [u8; 16],
    /// UUID assigned to the clone.
    pub clone_uuid: [u8; 16],
}

//--------------------------------------------------------------------------------------------------
// Functions
//--------------------------------------------------------------------------------------------------

/// Prove descriptor-safe formatting, Linux descriptor reopening, reflink isolation, and UUID
/// rewriting with valid ext4 metadata checksums.
pub fn run_descriptor_uuid_proof(work_dir: &Path) -> Result<DescriptorUuidProof, String> {
    std::fs::create_dir(work_dir).map_err(|error| {
        format!(
            "create exclusive proof directory {}: {error}",
            work_dir.display()
        )
    })?;
    let source_path = work_dir.join("source.ext4");
    let clone_path = work_dir.join("clone.ext4");

    let mut source = OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(&source_path)
        .map_err(|error| format!("create source image: {error}"))?;
    let before = source
        .metadata()
        .map_err(|error| format!("stat source descriptor before format: {error}"))?;

    format_ext4_file_with_uuid(
        &mut source,
        &Ext4FormatOptions {
            size_bytes: EXT4_IMAGE_BYTES,
            journal_blocks: EXT4_JOURNAL_BLOCKS,
        },
        SOURCE_UUID,
    )
    .map_err(|error| format!("format source descriptor: {error}"))?;

    let after = source
        .metadata()
        .map_err(|error| format!("stat source descriptor after format: {error}"))?;
    let named = std::fs::metadata(&source_path)
        .map_err(|error| format!("stat named source image: {error}"))?;
    if before.dev() != after.dev()
        || before.ino() != after.ino()
        || after.dev() != named.dev()
        || after.ino() != named.ino()
    {
        return Err("formatter replaced the parent-held source inode".to_string());
    }
    if after.len() != EXT4_IMAGE_BYTES {
        return Err(format!(
            "formatted logical size is {}, expected {EXT4_IMAGE_BYTES}",
            after.len()
        ));
    }

    let descriptor_path = format!("/proc/self/fd/{}", source.as_raw_fd());
    let mut reopened = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&descriptor_path)
        .map_err(|error| format!("reopen source through {descriptor_path}: {error}"))?;
    let reopened_metadata = reopened
        .metadata()
        .map_err(|error| format!("stat reopened source descriptor: {error}"))?;
    if reopened_metadata.dev() != after.dev() || reopened_metadata.ino() != after.ino() {
        return Err("/proc/self/fd reopening changed source inode identity".to_string());
    }
    if read_uuid(&mut reopened)? != SOURCE_UUID {
        return Err("reopened source descriptor has the wrong UUID".to_string());
    }

    let clone = OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(&clone_path)
        .map_err(|error| format!("create clone image: {error}"))?;
    clone
        .set_len(EXT4_IMAGE_BYTES)
        .map_err(|error| format!("size clone image: {error}"))?;
    reflink(&source, &clone)?;
    clone
        .sync_all()
        .map_err(|error| format!("flush reflink clone: {error}"))?;
    let clone_metadata = clone
        .metadata()
        .map_err(|error| format!("stat reflink clone: {error}"))?;
    if clone_metadata.dev() != after.dev() || clone_metadata.ino() == after.ino() {
        return Err(
            "FICLONE did not create an independent inode on the source filesystem".to_string(),
        );
    }
    drop(clone);

    let mut clone = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&clone_path)
        .map_err(|error| format!("reopen clone image: {error}"))?;
    rewrite_uuid(&mut clone, CLONE_UUID).map_err(|error| format!("rewrite clone UUID: {error}"))?;
    if read_uuid(&mut clone)? != CLONE_UUID {
        return Err("clone UUID rewrite did not persist".to_string());
    }
    if read_uuid(&mut source)? != SOURCE_UUID {
        return Err("clone UUID rewrite mutated the reflink source".to_string());
    }
    drop(clone);

    verify_e2fsck(&source_path)?;
    verify_e2fsck(&clone_path)?;

    Ok(DescriptorUuidProof {
        source_inode: after.ino(),
        clone_inode: clone_metadata.ino(),
        logical_bytes: after.len(),
        source_uuid: SOURCE_UUID,
        clone_uuid: CLONE_UUID,
    })
}

fn reflink(source: &File, destination: &File) -> Result<(), String> {
    // SAFETY: FICLONE reads the source fd passed as the third argument and writes into the valid,
    // open destination fd. Both files remain owned by the caller for the entire ioctl.
    let result = unsafe { libc::ioctl(destination.as_raw_fd(), FICLONE, source.as_raw_fd()) };
    if result == 0 {
        return Ok(());
    }
    Err(format!(
        "FICLONE source image: {}",
        std::io::Error::last_os_error()
    ))
}

fn read_uuid(file: &mut File) -> Result<[u8; 16], String> {
    let mut uuid = [0u8; 16];
    file.seek(SeekFrom::Start(EXT4_UUID_OFFSET))
        .map_err(|error| format!("seek ext4 UUID: {error}"))?;
    file.read_exact(&mut uuid)
        .map_err(|error| format!("read ext4 UUID: {error}"))?;
    Ok(uuid)
}

fn verify_e2fsck(path: &Path) -> Result<(), String> {
    let output = Command::new("e2fsck")
        .args(["-fn"])
        .arg(path)
        .output()
        .map_err(|error| format!("execute e2fsck for {}: {error}", path.display()))?;
    if output.status.success() {
        return Ok(());
    }
    let stderr = String::from_utf8_lossy(&output.stderr);
    let bounded = stderr.chars().take(2048).collect::<String>();
    Err(format!(
        "e2fsck rejected {} with {}: {}",
        path.display(),
        output.status,
        bounded.trim()
    ))
}

//--------------------------------------------------------------------------------------------------
// Tests
//--------------------------------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formatting_preserves_parent_descriptor_identity() {
        let parent = tempfile::tempdir().unwrap();
        let path = parent.path().join("source.ext4");
        let mut source = OpenOptions::new()
            .create_new(true)
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        let before = source.metadata().unwrap();

        format_ext4_file_with_uuid(
            &mut source,
            &Ext4FormatOptions {
                size_bytes: EXT4_IMAGE_BYTES,
                journal_blocks: EXT4_JOURNAL_BLOCKS,
            },
            SOURCE_UUID,
        )
        .unwrap();

        let after = source.metadata().unwrap();
        let named = std::fs::metadata(path).unwrap();
        assert_eq!((before.dev(), before.ino()), (after.dev(), after.ino()));
        assert_eq!((after.dev(), after.ino()), (named.dev(), named.ino()));
        assert_eq!(after.len(), EXT4_IMAGE_BYTES);
        assert_eq!(read_uuid(&mut source).unwrap(), SOURCE_UUID);
    }
}
