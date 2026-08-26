//! Single-client relay and typed Microsandbox agent session.

use std::{
    io::{Read, Write},
    os::unix::net::UnixStream,
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    thread,
    time::{Duration, Instant},
};

use microsandbox_agent_client::AgentClient;
use microsandbox_protocol::{
    codec,
    core::InitAck,
    exec::{
        ExecExited, ExecFailed, ExecFailureKind, ExecRequest, ExecResize, ExecSignal, ExecStderr,
        ExecStdin, ExecStdout,
    },
    fs::{
        FS_CHUNK_SIZE, FsData, FsEntryInfo, FsOp, FsOpenOptions, FsRequest, FsResponse,
        FsResponseData,
    },
    message::{Message, MessageType},
    tcp::{TcpClose, TcpConnect, TcpData, TcpEof},
};

use crate::console::AgentConsole;
use crate::protocol as helper;

const READY_TIMEOUT: Duration = Duration::from_secs(180);
const RELAY_ID_MIN: u32 = 1;
const RELAY_ID_MAX: u32 = u32::MAX / 16;

pub struct AgentSession {
    runtime: tokio::runtime::Runtime,
    client: Arc<AgentClient>,
    console: Arc<AgentConsole>,
    stopping: Arc<AtomicBool>,
}

pub struct AgentFileResult {
    pub content: Vec<u8>,
    pub metadata: Option<helper::FileMetadataEvent>,
}

pub struct AgentTcpSession<'a> {
    session: &'a AgentSession,
    id: u32,
    events: tokio::sync::mpsc::Receiver<Message>,
}

pub struct AgentExecSession<'a> {
    session: &'a AgentSession,
    id: u32,
    events: tokio::sync::mpsc::Receiver<Message>,
}

impl AgentExecSession<'_> {
    pub fn next(&mut self, timeout: Duration) -> Result<Option<Message>, String> {
        self.session.runtime.block_on(async {
            match tokio::time::timeout(timeout, self.events.recv()).await {
                Ok(value) => Ok(value),
                Err(_) => Ok(None),
            }
        })
    }

    pub fn stdin(&self, data: Vec<u8>) -> Result<(), String> {
        self.session
            .runtime
            .block_on(self.session.client.send(
                self.id,
                MessageType::ExecStdin,
                &ExecStdin { data },
            ))
            .map_err(|error| format!("send Exec stdin: {error}"))
    }

    pub fn resize(&self, rows: u32, columns: u32) -> Result<(), String> {
        let rows = u16::try_from(rows).map_err(|_| "PTY rows exceed u16")?;
        let cols = u16::try_from(columns).map_err(|_| "PTY columns exceed u16")?;
        self.session
            .runtime
            .block_on(self.session.client.send(
                self.id,
                MessageType::ExecResize,
                &ExecResize { rows, cols },
            ))
            .map_err(|error| format!("resize Exec PTY: {error}"))
    }

    pub fn signal(&self, signal: i32) -> Result<(), String> {
        self.session
            .runtime
            .block_on(self.session.client.send(
                self.id,
                MessageType::ExecSignal,
                &ExecSignal { signal },
            ))
            .map_err(|error| format!("signal Exec: {error}"))
    }
}

impl AgentTcpSession<'_> {
    pub fn next(&mut self, timeout: Duration) -> Result<Option<Message>, String> {
        self.session.runtime.block_on(async {
            match tokio::time::timeout(timeout, self.events.recv()).await {
                Ok(value) => Ok(value),
                Err(_) => Ok(None),
            }
        })
    }

    pub fn send(&self, data: Vec<u8>) -> Result<(), String> {
        self.session
            .runtime
            .block_on(
                self.session
                    .client
                    .send(self.id, MessageType::TcpData, &TcpData { data }),
            )
            .map_err(|error| format!("send TCP data: {error}"))
    }

    pub fn eof(&self) -> Result<(), String> {
        self.session
            .runtime
            .block_on(
                self.session
                    .client
                    .send(self.id, MessageType::TcpEof, &TcpEof {}),
            )
            .map_err(|error| format!("send TCP EOF: {error}"))
    }

    pub fn close(&self) -> Result<(), String> {
        self.session
            .runtime
            .block_on(
                self.session
                    .client
                    .send(self.id, MessageType::TcpClose, &TcpClose {}),
            )
            .map_err(|error| format!("close TCP session: {error}"))
    }
}

impl AgentSession {
    pub fn connect(console: Arc<AgentConsole>) -> Result<Self, String> {
        let (ready_frame, trailing) = wait_ready(&console)?;
        let (client_stream, mut relay_stream) =
            UnixStream::pair().map_err(|error| format!("create internal agent relay: {error}"))?;
        relay_stream
            .write_all(&RELAY_ID_MIN.to_be_bytes())
            .and_then(|()| relay_stream.write_all(&RELAY_ID_MAX.to_be_bytes()))
            .and_then(|()| relay_stream.write_all(&ready_frame))
            .map_err(|error| format!("write internal agent handshake: {error}"))?;

        let stopping = Arc::new(AtomicBool::new(false));
        start_guest_to_client(
            Arc::clone(&console),
            relay_stream
                .try_clone()
                .map_err(|error| format!("clone internal agent relay: {error}"))?,
            trailing,
            Arc::clone(&stopping),
        )?;
        start_client_to_guest(Arc::clone(&console), relay_stream, Arc::clone(&stopping))?;

        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .map_err(|error| format!("create agent runtime: {error}"))?;
        client_stream
            .set_nonblocking(true)
            .map_err(|error| format!("make internal agent client nonblocking: {error}"))?;
        let async_stream = {
            let _guard = runtime.enter();
            tokio::net::UnixStream::from_std(client_stream)
                .map_err(|error| format!("adopt internal agent client: {error}"))?
        };
        let client = runtime
            .block_on(AgentClient::connect_stream_with_timeout(
                async_stream,
                Duration::from_secs(10),
            ))
            .map_err(|error| format!("negotiate agent protocol: {error}"))?;
        Ok(Self {
            runtime,
            client: Arc::new(client),
            console,
            stopping,
        })
    }

    pub fn protocol_generation(&self) -> u32 {
        u32::from(self.client.negotiated_version())
    }

    pub fn agent_version(&self) -> String {
        self.client.agent_version().to_owned()
    }

    pub fn probe_workspace(&self) -> Result<(), String> {
        let request = ExecRequest {
            cmd: "/bin/sh".into(),
            args: vec![
                "-c".into(),
                "set -eu; test -d /workspace; test -w /workspace; printf secondbox-workspace-ready > /workspace/.secondbox-ready; test \"$(cat /workspace/.secondbox-ready)\" = secondbox-workspace-ready; rm /workspace/.secondbox-ready; printf secondbox-workspace-ready".into(),
            ],
            env: Vec::new(),
            cwd: Some("/workspace".into()),
            user: None,
            tty: false,
            rows: 24,
            cols: 80,
            rlimits: Vec::new(),
        };
        self.runtime.block_on(async {
            let (_, mut events) = self
                .client
                .stream(MessageType::ExecRequest, &request)
                .await
                .map_err(|error| format!("start Workspace readiness probe: {error}"))?;
            let mut stdout = Vec::new();
            let mut stderr = Vec::new();
            while let Some(event) = events.recv().await {
                match event.t {
                    MessageType::ExecStarted => {}
                    MessageType::ExecStdout => stdout.extend(
                        event
                            .payload::<ExecStdout>()
                            .map_err(|error| format!("decode Workspace probe stdout: {error}"))?
                            .data,
                    ),
                    MessageType::ExecStderr => stderr.extend(
                        event
                            .payload::<ExecStderr>()
                            .map_err(|error| format!("decode Workspace probe stderr: {error}"))?
                            .data,
                    ),
                    MessageType::ExecExited => {
                        let exited: ExecExited = event
                            .payload()
                            .map_err(|error| format!("decode Workspace probe exit: {error}"))?;
                        if exited.code != 0 {
                            return Err(format!(
                                "Workspace readiness probe exited {}: {}",
                                exited.code,
                                String::from_utf8_lossy(&stderr)
                            ));
                        }
                        if stdout != b"secondbox-workspace-ready" {
                            return Err(
                                "Workspace readiness probe returned unexpected output".into()
                            );
                        }
                        return Ok(());
                    }
                    MessageType::ExecFailed => {
                        let failed: ExecFailed = event
                            .payload()
                            .map_err(|error| format!("decode Workspace probe failure: {error}"))?;
                        return Err(format!(
                            "Workspace readiness probe failed: {}",
                            failed.message
                        ));
                    }
                    other => {
                        return Err(format!(
                            "Workspace readiness probe received unexpected event {}",
                            other.as_str()
                        ));
                    }
                }
            }
            Err("Workspace readiness probe agent stream closed without a terminal event".into())
        })
    }

    pub fn start_exec(
        &self,
        request: &helper::ExecRequest,
    ) -> Result<AgentExecSession<'_>, String> {
        if request.argv.is_empty() || request.argv[0].is_empty() {
            return Err("Exec argv is empty".into());
        }
        let rows = u16::try_from(request.rows).map_err(|_| "PTY rows exceed u16")?;
        let cols = u16::try_from(request.columns).map_err(|_| "PTY columns exceed u16")?;
        let exec = ExecRequest {
            cmd: request.argv[0].clone(),
            args: request.argv[1..].to_vec(),
            env: request.environment.clone(),
            cwd: (!request.working_directory.is_empty()).then(|| request.working_directory.clone()),
            user: None,
            tty: request.pty,
            rows: if request.pty { rows } else { 24 },
            cols: if request.pty { cols } else { 80 },
            rlimits: Vec::new(),
        };
        let (id, events) = self
            .runtime
            .block_on(self.client.stream(MessageType::ExecRequest, &exec))
            .map_err(|error| format!("start Exec: {error}"))?;
        Ok(AgentExecSession {
            session: self,
            id,
            events,
        })
    }

    pub fn preflight_exec(
        &self,
        request: &helper::ExecRequest,
    ) -> Result<Option<ExecFailureKind>, String> {
        let command = request.argv.first().ok_or("Exec argv is empty")?;
        if !command.contains('/') {
            return Ok(None);
        }
        let command_path = Path::new(command);
        let path = if command_path.is_absolute() || request.working_directory.is_empty() {
            command_path.to_path_buf()
        } else {
            Path::new(&request.working_directory).join(command_path)
        };
        let path = path
            .to_str()
            .ok_or("Exec command path is not valid UTF-8")?
            .to_owned();
        self.runtime.block_on(async {
            match fs_request(
                &self.client,
                FsOp::Stat {
                    path,
                    follow_symlink: true,
                },
            )
            .await
            {
                Ok(response) => match response.data {
                    Some(FsResponseData::Stat(entry)) if entry.kind == "dir" => {
                        Ok(Some(ExecFailureKind::NotExecutable))
                    }
                    Some(FsResponseData::Stat(entry)) if entry.mode & 0o111 == 0 => {
                        Ok(Some(ExecFailureKind::PermissionDenied))
                    }
                    Some(FsResponseData::Stat(_)) => Ok(None),
                    _ => Err("Exec command preflight returned no metadata".into()),
                },
                Err(error) if file_not_found(&error) => Ok(Some(ExecFailureKind::NotFound)),
                Err(error) if file_permission_denied(&error) => {
                    Ok(Some(ExecFailureKind::PermissionDenied))
                }
                Err(error) => Err(format!("Exec command preflight: {error}")),
            }
        })
    }

    pub fn file(&self, request: &helper::FileRequest) -> Result<AgentFileResult, String> {
        let path = workspace_path(&request.path)?;
        self.runtime.block_on(async {
            use helper::Operation;
            match Operation::try_from(request.operation).unwrap_or(Operation::Unspecified) {
                Operation::FileRead => {
                    validate_workspace_path(&self.client, &path, false).await?;
                    let handle = fs_handle(
                        &self.client,
                        FsOp::OpenFile {
                            path,
                            options: FsOpenOptions {
                                read: true,
                                ..Default::default()
                            },
                        },
                    )
                    .await?;
                    let result = async {
                        let req = FsRequest {
                            op: FsOp::Read {
                                handle,
                                offset: request.offset,
                                len: (request.limit != 0).then_some(request.limit),
                            },
                        };
                        let (_, mut events) = self
                            .client
                            .stream(MessageType::FsRequest, &req)
                            .await
                            .map_err(|e| format!("read file: {e}"))?;
                        let mut content = Vec::new();
                        while let Some(event) = events.recv().await {
                            match event.t {
                                MessageType::FsData => content.extend(
                                    event
                                        .payload::<FsData>()
                                        .map_err(|e| format!("decode file data: {e}"))?
                                        .data,
                                ),
                                MessageType::FsResponse => {
                                    fs_ok(
                                        event
                                            .payload()
                                            .map_err(|e| format!("decode file response: {e}"))?,
                                    )?;
                                    return Ok(AgentFileResult {
                                        content,
                                        metadata: None,
                                    });
                                }
                                other => {
                                    return Err(format!(
                                        "file read received unexpected event {}",
                                        other.as_str()
                                    ));
                                }
                            }
                        }
                        Err("file read closed without terminal response".into())
                    }
                    .await;
                    let _ = fs_request(&self.client, FsOp::CloseHandle { handle }).await;
                    result
                }
                Operation::FileWrite => {
                    validate_workspace_path(&self.client, &path, true).await?;
                    // Exclusive creation is the only open in this protocol
                    // that cannot traverse a leaf symlink. Remove whatever
                    // leaf exists (removal unlinks a symlink itself, never
                    // its target), then create the file exclusively so a
                    // raced-in replacement fails the open instead of
                    // redirecting the write outside /workspace. The agentd
                    // filesystem protocol is pathname-only, so a concurrent
                    // parent-directory swap remains a guest-internal race a
                    // process already executing inside the guest could win;
                    // beneath/no-follow semantics need a protocol revision
                    // and are recorded as a known limitation.
                    let _ = fs_request(&self.client, FsOp::Remove { path: path.clone() }).await;
                    let handle = fs_handle(
                        &self.client,
                        FsOp::OpenFile {
                            path,
                            options: FsOpenOptions {
                                write: true,
                                create: true,
                                create_new: true,
                                mode: Some(request.mode),
                                ..Default::default()
                            },
                        },
                    )
                    .await?;
                    let result = async {
                        let req = FsRequest {
                            op: FsOp::Write {
                                handle,
                                offset: request.offset,
                                len: Some(request.content.len() as u64),
                            },
                        };
                        let (id, mut events) = self
                            .client
                            .stream(MessageType::FsRequest, &req)
                            .await
                            .map_err(|e| format!("write file: {e}"))?;
                        for chunk in request.content.chunks(FS_CHUNK_SIZE) {
                            self.client
                                .send(
                                    id,
                                    MessageType::FsData,
                                    &FsData {
                                        data: chunk.to_vec(),
                                    },
                                )
                                .await
                                .map_err(|e| format!("write file data: {e}"))?;
                        }
                        self.client
                            .send(id, MessageType::FsData, &FsData { data: Vec::new() })
                            .await
                            .map_err(|e| format!("finish file data: {e}"))?;
                        while let Some(event) = events.recv().await {
                            if event.t == MessageType::FsResponse {
                                fs_ok(
                                    event
                                        .payload()
                                        .map_err(|e| format!("decode file response: {e}"))?,
                                )?;
                                return Ok(AgentFileResult {
                                    content: Vec::new(),
                                    metadata: None,
                                });
                            }
                        }
                        Err("file write closed without terminal response".into())
                    }
                    .await;
                    let _ = fs_request(&self.client, FsOp::CloseHandle { handle }).await;
                    result
                }
                Operation::FileStat => {
                    validate_workspace_path(&self.client, &path, false).await?;
                    fs_metadata(
                        &self.client,
                        FsOp::Stat {
                            path,
                            follow_symlink: false,
                        },
                    )
                    .await
                }
                Operation::FileExists => {
                    let stat = async {
                        validate_workspace_path(&self.client, &path, false).await?;
                        fs_metadata(
                            &self.client,
                            FsOp::Stat {
                                path,
                                follow_symlink: false,
                            },
                        )
                        .await
                    }
                    .await;
                    match stat {
                        Ok(result) => Ok(result),
                        Err(error) if file_not_found(&error) => Ok(AgentFileResult {
                            content: Vec::new(),
                            metadata: Some(helper::FileMetadataEvent {
                                exists: false,
                                ..Default::default()
                            }),
                        }),
                        Err(error) => Err(error),
                    }
                }
                Operation::FileList => {
                    validate_workspace_path(&self.client, &path, false).await?;
                    fs_metadata(&self.client, FsOp::List { path }).await
                }
                Operation::FileMkdir if request.recursive => {
                    mkdir_all(&self.client, &path, request.mode).await?;
                    Ok(empty_file_result())
                }
                Operation::FileMkdir => {
                    validate_workspace_path(&self.client, &path, true).await?;
                    fs_request(
                        &self.client,
                        FsOp::Mkdir {
                            path,
                            mode: Some(request.mode),
                        },
                    )
                    .await?;
                    Ok(empty_file_result())
                }
                Operation::FileRemove if request.recursive => {
                    let removal = async {
                        validate_workspace_path(&self.client, &path, false).await?;
                        fs_request(
                            &self.client,
                            FsOp::RemoveDir {
                                path,
                                recursive: true,
                            },
                        )
                        .await
                    }
                    .await;
                    match removal {
                        Ok(_) => Ok(empty_file_result()),
                        Err(error) if request.force && file_not_found(&error) => {
                            Ok(empty_file_result())
                        }
                        Err(error) => Err(error),
                    }
                }
                Operation::FileRemove => {
                    let removal = async {
                        validate_workspace_path(&self.client, &path, false).await?;
                        fs_request(&self.client, FsOp::Remove { path }).await
                    }
                    .await;
                    match removal {
                        Ok(_) => Ok(empty_file_result()),
                        Err(error) if request.force && file_not_found(&error) => {
                            Ok(empty_file_result())
                        }
                        Err(error) => Err(error),
                    }
                }
                _ => Err("unsupported helper File operation".into()),
            }
        })
    }

    pub fn tcp(&self, request: &helper::TcpRequest) -> Result<AgentTcpSession<'_>, String> {
        let port = u16::try_from(request.port).map_err(|_| "TCP port exceeds u16")?;
        let (id, events) = self
            .runtime
            .block_on(self.client.stream(
                MessageType::TcpConnect,
                &TcpConnect {
                    host: request.host.clone(),
                    port,
                },
            ))
            .map_err(|error| format!("connect guest TCP: {error}"))?;
        Ok(AgentTcpSession {
            session: self,
            id,
            events,
        })
    }

    pub fn shutdown(&self) -> Result<(), String> {
        let message = Message::with_payload(MessageType::Shutdown, 0, &())
            .map_err(|error| format!("encode guest shutdown: {error}"))?;
        let mut frame = Vec::new();
        codec::encode_to_buf(&message, &mut frame)
            .map_err(|error| format!("frame guest shutdown: {error}"))?;
        self.console
            .push_to_guest(frame)
            .map_err(|error| format!("request guest shutdown: {error}"))
    }
}

fn workspace_path(relative: &str) -> Result<String, String> {
    if relative.is_empty()
        || relative.starts_with('/')
        || relative
            .split('/')
            .any(|part| part.is_empty() || part == "." || part == "..")
    {
        return Err("workspace-relative path is invalid".into());
    }
    Ok(format!("/workspace/{relative}"))
}

async fn validate_workspace_path(
    client: &AgentClient,
    path: &str,
    allow_missing_leaf: bool,
) -> Result<(), String> {
    if !allow_missing_leaf {
        let response = fs_request(
            client,
            FsOp::Stat {
                path: path.into(),
                follow_symlink: false,
            },
        )
        .await?;
        if !matches!(response.data, Some(FsResponseData::Stat(_))) {
            return Err("workspace-relative path stat returned no metadata".into());
        }
    }
    let candidate = if allow_missing_leaf {
        Path::new(path)
            .parent()
            .and_then(Path::to_str)
            .ok_or("workspace-relative path parent is invalid")?
    } else {
        path
    };
    let response = fs_request(
        client,
        FsOp::RealPath {
            path: candidate.into(),
        },
    )
    .await?;
    match response.data {
        Some(FsResponseData::Path(resolved))
            if resolved == candidate
                && (resolved == "/workspace" || resolved.starts_with("/workspace/")) =>
        {
            Ok(())
        }
        Some(FsResponseData::Path(_)) => {
            Err("workspace-relative path resolves through a symbolic link".into())
        }
        _ => Err("workspace-relative path resolution returned no path".into()),
    }
}

async fn mkdir_all(client: &AgentClient, path: &str, mode: u32) -> Result<(), String> {
    let relative = path
        .strip_prefix("/workspace/")
        .ok_or("workspace-relative mkdir path is invalid")?;
    let mut current = String::from("/workspace");
    for component in relative.split('/') {
        current.push('/');
        current.push_str(component);
        match validate_workspace_path(client, &current, false).await {
            Ok(()) => continue,
            Err(error) if file_not_found(&error) => {
                validate_workspace_path(client, &current, true).await?;
                match fs_request(
                    client,
                    FsOp::Mkdir {
                        path: current.clone(),
                        mode: Some(mode),
                    },
                )
                .await
                {
                    Ok(_) => {}
                    Err(error) if file_already_exists(&error) => {}
                    Err(error) => return Err(error),
                }
            }
            Err(error) => return Err(error),
        }
    }
    Ok(())
}

fn file_not_found(error: &str) -> bool {
    let value = error.to_ascii_lowercase();
    value.contains("enoent")
        || value.contains("not found")
        || value.contains("no such file or directory")
        || value.contains("os error 2")
}

fn file_already_exists(error: &str) -> bool {
    let value = error.to_ascii_lowercase();
    value.contains("eexist") || value.contains("already exists") || value.contains("os error 17")
}

fn file_permission_denied(error: &str) -> bool {
    let value = error.to_ascii_lowercase();
    value.contains("eacces")
        || value.contains("eperm")
        || value.contains("permission denied")
        || value.contains("os error 13")
}

fn empty_file_result() -> AgentFileResult {
    AgentFileResult {
        content: Vec::new(),
        metadata: None,
    }
}

async fn fs_request(client: &AgentClient, op: FsOp) -> Result<FsResponse, String> {
    let response: FsResponse = client
        .request(MessageType::FsRequest, &FsRequest { op })
        .await
        .map_err(|e| format!("filesystem request: {e}"))?
        .payload()
        .map_err(|e| format!("decode filesystem response: {e}"))?;
    fs_ok(response)
}

fn fs_ok(response: FsResponse) -> Result<FsResponse, String> {
    if response.ok {
        Ok(response)
    } else {
        Err(response
            .error
            .unwrap_or_else(|| "filesystem operation failed".into()))
    }
}

async fn fs_handle(client: &AgentClient, op: FsOp) -> Result<u64, String> {
    match fs_request(client, op).await?.data {
        Some(FsResponseData::Handle(handle)) => Ok(handle),
        _ => Err("filesystem open returned no handle".into()),
    }
}

async fn fs_metadata(client: &AgentClient, op: FsOp) -> Result<AgentFileResult, String> {
    let response = fs_request(client, op).await?;
    let mut metadata = helper::FileMetadataEvent {
        exists: true,
        ..Default::default()
    };
    match response.data {
        Some(FsResponseData::Stat(entry)) => apply_entry(&mut metadata, &entry),
        Some(FsResponseData::List(entries)) => {
            metadata.kind = 2;
            for entry in entries {
                let relative = entry
                    .path
                    .strip_prefix("/workspace/")
                    .ok_or("filesystem list returned an entry outside /workspace")?;
                let child = Path::new(relative)
                    .file_name()
                    .and_then(|value| value.to_str())
                    .ok_or("filesystem list returned an invalid child name")?;
                metadata.direct_children.push(child.into());
                metadata
                    .direct_child_entries
                    .push(helper::FileMetadataEntry {
                        path: relative.into(),
                        kind: file_kind(&entry.kind),
                        size: entry.size,
                        modified_at_unix_ms: entry.modified.unwrap_or_default().max(0) as u64
                            * 1000,
                    });
            }
        }
        _ => {}
    }
    Ok(AgentFileResult {
        content: Vec::new(),
        metadata: Some(metadata),
    })
}

fn apply_entry(metadata: &mut helper::FileMetadataEvent, entry: &FsEntryInfo) {
    metadata.size = entry.size;
    metadata.mode = entry.mode;
    metadata.kind = file_kind(&entry.kind);
    metadata.modified_at_unix_ms = entry.modified.unwrap_or_default().max(0) as u64 * 1000;
}

fn file_kind(kind: &str) -> u32 {
    match kind {
        "file" => 1,
        "dir" => 2,
        "symlink" => 3,
        _ => 0,
    }
}

impl Drop for AgentSession {
    fn drop(&mut self) {
        self.stopping.store(true, Ordering::Release);
    }
}

fn wait_ready(console: &AgentConsole) -> Result<(Vec<u8>, Vec<u8>), String> {
    let deadline = Instant::now() + READY_TIMEOUT;
    let mut buffered = Vec::new();
    loop {
        while let Some(frame) = codec::try_decode_raw_from_buf(&mut buffered)
            .map_err(|error| format!("decode pre-ready agent frame: {error}"))?
        {
            let message = codec::raw_frame_to_message(frame)
                .map_err(|error| format!("decode pre-ready agent message: {error}"))?;
            match message.t {
                MessageType::InitResolved => {
                    let ack = Message::with_payload(MessageType::InitAck, 0, &InitAck {})
                        .map_err(|error| format!("encode init acknowledgement: {error}"))?;
                    let mut bytes = Vec::new();
                    codec::encode_to_buf(&ack, &mut bytes)
                        .map_err(|error| format!("frame init acknowledgement: {error}"))?;
                    console
                        .push_to_guest(bytes)
                        .map_err(|error| format!("send init acknowledgement: {error}"))?;
                }
                MessageType::Ready => {
                    let mut ready = Vec::new();
                    codec::encode_to_buf(&message, &mut ready)
                        .map_err(|error| format!("preserve ready frame: {error}"))?;
                    return Ok((ready, buffered));
                }
                other => {
                    return Err(format!(
                        "unexpected pre-ready agent message {}",
                        other.as_str()
                    ));
                }
            }
        }
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err("timed out waiting for Microsandbox agent readiness".into());
        }
        if let Some(bytes) = console
            .take_from_guest(remaining.min(Duration::from_secs(1)))
            .map_err(|error| format!("wait for Microsandbox agent: {error}"))?
        {
            buffered.extend(bytes);
        }
    }
}

fn start_guest_to_client(
    console: Arc<AgentConsole>,
    mut stream: UnixStream,
    trailing: Vec<u8>,
    stopping: Arc<AtomicBool>,
) -> Result<(), String> {
    thread::Builder::new()
        .name("secondbox-agent-guest-to-client".into())
        .spawn(move || {
            if !trailing.is_empty() && stream.write_all(&trailing).is_err() {
                stopping.store(true, Ordering::Release);
                return;
            }
            while !stopping.load(Ordering::Acquire) {
                match console.take_from_guest(Duration::from_millis(250)) {
                    Ok(Some(bytes)) if stream.write_all(&bytes).is_err() => break,
                    Ok(_) => {}
                    Err(_) => break,
                }
            }
            stopping.store(true, Ordering::Release);
        })
        .map(|_| ())
        .map_err(|error| format!("start guest-to-client relay: {error}"))
}

fn start_client_to_guest(
    console: Arc<AgentConsole>,
    mut stream: UnixStream,
    stopping: Arc<AtomicBool>,
) -> Result<(), String> {
    thread::Builder::new()
        .name("secondbox-agent-client-to-guest".into())
        .spawn(move || {
            let mut length = [0_u8; 4];
            while !stopping.load(Ordering::Acquire) {
                if stream.read_exact(&mut length).is_err() {
                    break;
                }
                let frame_length = u32::from_be_bytes(length) as usize;
                if frame_length > microsandbox_protocol::codec::MAX_FRAME_SIZE as usize {
                    break;
                }
                let mut bytes = Vec::with_capacity(4 + frame_length);
                bytes.extend_from_slice(&length);
                bytes.resize(4 + frame_length, 0);
                if stream.read_exact(&mut bytes[4..]).is_err()
                    || console.push_to_guest(bytes).is_err()
                {
                    break;
                }
            }
            stopping.store(true, Ordering::Release);
        })
        .map(|_| ())
        .map_err(|error| format!("start client-to-guest relay: {error}"))
}
