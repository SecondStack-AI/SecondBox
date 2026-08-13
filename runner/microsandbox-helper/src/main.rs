use std::{
    fs::{File, OpenOptions},
    io::Read,
    os::{
        fd::{FromRawFd, RawFd},
        unix::net::UnixStream,
    },
    process,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    thread,
    time::Duration,
};

use secondbox_microsandbox_helper::{
    HELPER_VERSION, PROTOCOL_VERSION,
    agent::AgentSession,
    bounded_diagnostic,
    console::AgentConsole,
    frame::{read_frame, write_frame},
    protocol::{self, DiagnosticEvent, Envelope, ReadyEvent, StreamChannel, StreamData, TerminalEvent, envelope::Message},
    state::ProtocolState,
    vmm::LaunchConfiguration,
    workspace::format_workspace,
};
use microsandbox_protocol::{message::MessageType, tcp::{TcpData, TcpFailed}};

const CONTROL_FD: RawFd = 3;
const WORKSPACE_FD: RawFd = 4;
const LIFECYCLE_FD: RawFd = 5;
const PARENT_LOSS_FLUSH_BOUND: Duration = Duration::from_secs(4);

fn main() {
    let action = std::env::args().nth(1).unwrap_or_default();
    let result = match action.as_str() {
        "serve" => serve(),
        "format-workspace" => serve_format_only(),
        _ => Err("usage: secondbox-microsandbox-helper serve|format-workspace".into()),
    };
    if let Err(error) = result {
        eprintln!(
            "SecondBox Microsandbox helper: {}",
            bounded_diagnostic(&error)
        );
        process::exit(1);
    }
}

fn serve() -> Result<(), String> {
    let mut control = inherited_socket(CONTROL_FD)?;
    let workspace = inherited_file(WORKSPACE_FD, true)?;
    let shutting_down = Arc::new(AtomicBool::new(false));
    monitor_parent(
        shutting_down.clone(),
        workspace.try_clone().map_err(|error| error.to_string())?,
    )?;
    let envelope = read_frame(&mut control)
        .map_err(|error| error.to_string())?
        .ok_or("start request is required")?;
    let mut state = ProtocolState::default();
    state.admit(&envelope).map_err(|error| error.to_string())?;
    let Some(Message::Start(start)) = envelope.message.as_ref() else {
        return Err("first helper request must be start".into());
    };
    let materialization_digest = start.materialization_digest.clone();
    let start_request_id = envelope.request_id;
    let launch = LaunchConfiguration::from_start(start).map_err(|error| error.to_string())?;
    let console = AgentConsole::new().map_err(|error| format!("create agent console: {error}"))?;
    let runtime_dir = tempfile::Builder::new()
        .prefix("secondbox-microsandbox-runtime-")
        .tempdir()
        .map_err(|error| format!("create private guest runtime directory: {error}"))?;
    let vm = launch
        .build(console.backend(), runtime_dir.path())
        .map_err(|error| format!("SecondBox Microsandbox helper libkrun configuration: {error}"))?;
    let exit_handle = vm.exit_handle();
    thread::Builder::new()
        .name("secondbox-helper-control".into())
        .spawn(move || {
            if let Err(error) = supervise_instance(
                control,
                workspace,
                console,
                state,
                start_request_id,
                materialization_digest,
                exit_handle,
                shutting_down,
            ) {
                eprintln!(
                    "SecondBox Microsandbox helper control: {}",
                    bounded_diagnostic(&error)
                );
                process::exit(1);
            }
        })
        .map_err(|error| format!("start helper control thread: {error}"))?;
    match vm.enter() {
        Ok(never) => match never {},
        Err(error) => Err(format!("enter Microsandbox VM: {error}")),
    }
}

#[allow(clippy::too_many_arguments)]
fn supervise_instance(
    mut control: UnixStream,
    workspace: File,
    console: Arc<AgentConsole>,
    mut state: ProtocolState,
    start_request_id: u64,
    materialization_digest: String,
    exit_handle: msb_krun::ExitHandle,
    shutting_down: Arc<AtomicBool>,
) -> Result<(), String> {
    let agent = AgentSession::connect(console)?;
    agent.probe_workspace()?;
    write_frame(
        &mut control,
        &Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: start_request_id,
            message: Some(Message::Ready(ReadyEvent {
                helper_version: HELPER_VERSION.into(),
                dependency_version: format!(
                    "microsandbox-0.6.8/msb_krun-0.1.30/agentd-{}",
                    agent.agent_version()
                ),
                host_platform: format!("{}-{}", std::env::consts::OS, std::env::consts::ARCH),
                agent_protocol_generation: agent.protocol_generation(),
                agent_features: vec![
                    "exec-streaming".into(),
                    "file-streaming".into(),
                    "pty".into(),
                    "tcp".into(),
                ],
                operations: supported_operations()
                    .into_iter()
                    .map(|operation| operation as i32)
                    .collect(),
                materialization_digest,
            })),
            ..Default::default()
        },
    )
    .map_err(|error| error.to_string())?;

    while !shutting_down.load(Ordering::Acquire) {
        let Some(envelope) = read_frame(&mut control).map_err(|error| error.to_string())? else {
            return Err("runner control socket closed while the Instance was active".into());
        };
        if let Err(error) = state.admit(&envelope) {
            write_diagnostic(
                &mut control,
                &envelope,
                "protocol_rejected",
                &error.to_string(),
            )?;
            continue;
        }
        match envelope.message.as_ref() {
            Some(Message::Shutdown(request)) => {
                agent.shutdown()?;
                workspace.sync_all().map_err(|error| error.to_string())?;
                write_terminal(&mut control, &envelope, true, 0, "shutdown-requested")?;
                let now_ms = std::time::SystemTime::now()
                    .duration_since(std::time::SystemTime::UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_millis() as u64;
                let delay = Duration::from_millis(
                    request
                        .flush_deadline_unix_ms
                        .saturating_sub(now_ms)
                        .min(4_000),
                );
                let final_workspace = workspace
                    .try_clone()
                    .map_err(|error| format!("clone Workspace for forced shutdown flush: {error}"))?;
                thread::spawn(move || {
                    thread::sleep(delay);
                    exit_handle.trigger();
                    thread::sleep(Duration::from_millis(250));
                    let _ = final_workspace.sync_all();
                    process::exit(0);
                });
                return Ok(());
            }
            Some(Message::Exec(request)) => {
                let result = agent.execute(request)?;
                if !result.stdout.is_empty() {
                    write_data(&mut control, &envelope, StreamChannel::Stdout, result.stdout, false)?;
                }
                if !result.stderr.is_empty() {
                    write_data(&mut control, &envelope, StreamChannel::Stderr, result.stderr, false)?;
                }
                if let Some(failed) = result.failed {
                    use microsandbox_protocol::exec::ExecFailureKind;
                    let reason = match failed.kind {
                        ExecFailureKind::NotFound => 1,
                        ExecFailureKind::PermissionDenied => 2,
                        ExecFailureKind::BadCwd => 3,
                        ExecFailureKind::NotExecutable => 4,
                        _ => 0,
                    };
                    write_exec_terminal(&mut control, &envelope, false, -1, "spawn-failed", reason)?;
                } else {
                    write_exec_terminal(&mut control, &envelope, true, result.exit_code, "exited", 0)?;
                }
            }
            Some(Message::File(request)) => {
                match agent.file(request) {
                    Ok(result) => {
                        if let Some(metadata) = result.metadata {
                            write_frame(&mut control, &Envelope {
                                protocol_version: PROTOCOL_VERSION,
                                request_id: envelope.request_id,
                                stream_id: envelope.stream_id,
                                message: Some(Message::FileMetadata(metadata)),
                                ..Default::default()
                            }).map_err(|error| error.to_string())?;
                        }
                        if !result.content.is_empty() {
                            write_data(&mut control, &envelope, StreamChannel::Stdout, result.content, true)?;
                        }
                        write_terminal(&mut control, &envelope, true, 0, "completed")?;
                    }
                    Err(error) => write_terminal(&mut control, &envelope, false, -1, &format!("file-failed:{error}"))?,
                }
            }
            Some(Message::Tcp(request)) => relay_tcp(&agent, &mut control, &mut state, &envelope, request)?,
            Some(Message::Cancel(_))
            | Some(Message::Pty(_))
            | Some(Message::StreamData(_))
            | Some(Message::StreamCredit(_)) => write_diagnostic(
                &mut control,
                &envelope,
                "operation_pending_data_plane",
                "operation is admitted but not implemented by this helper build",
            )?,
            _ => write_diagnostic(
                &mut control,
                &envelope,
                "invalid_direction",
                "event is not accepted from the runner",
            )?,
        }
    }
    Err("helper lifecycle monitor requested shutdown".into())
}

fn relay_tcp(
    agent: &AgentSession,
    control: &mut UnixStream,
    _state: &mut ProtocolState,
    request_envelope: &Envelope,
    request: &protocol::TcpRequest,
) -> Result<(), String> {
    let mut tcp = agent.tcp(request)?;
    let mut next_sequence = 1_u64;
    loop {
        if let Some(event) = tcp.next(Duration::from_millis(10))? {
            match event.t {
                MessageType::TcpConnected => {
                    write_frame(control, &Envelope {
                        protocol_version: PROTOCOL_VERSION,
                        request_id: request_envelope.request_id,
                        stream_id: request_envelope.stream_id,
                        message: Some(Message::TcpConnected(protocol::TcpConnectedEvent {})),
                        ..Default::default()
                    }).map_err(|error| error.to_string())?;
                }
                MessageType::TcpData => write_data(
                    control, request_envelope, StreamChannel::Tcp,
                    event.payload::<TcpData>().map_err(|error| format!("decode TCP data: {error}"))?.data,
                    false,
                )?,
                MessageType::TcpEof => write_data(control, request_envelope, StreamChannel::Tcp, Vec::new(), true)?,
                MessageType::TcpClosed => {
                    write_terminal(control, request_envelope, true, 0, "tcp-closed")?;
                    return Ok(());
                }
                MessageType::TcpFailed => {
                    let failed: TcpFailed = event.payload().map_err(|error| format!("decode TCP failure: {error}"))?;
                    write_terminal(control, request_envelope, false, -1, &format!("tcp-failed:{}", failed.error))?;
                    return Ok(());
                }
                other => return Err(format!("TCP received unexpected agent event {}", other.as_str())),
            }
        }
        let mut descriptor = libc::pollfd { fd: CONTROL_FD, events: libc::POLLIN, revents: 0 };
        let ready = unsafe { libc::poll(&mut descriptor, 1, 0) };
        if ready < 0 { return Err(format!("poll TCP control: {}", std::io::Error::last_os_error())); }
        if ready == 0 { continue; }
        let incoming = read_frame(control).map_err(|error| error.to_string())?
            .ok_or("runner control socket closed during TCP relay")?;
        if incoming.request_id != request_envelope.request_id
            || incoming.stream_id != request_envelope.request_id
            || incoming.sequence != next_sequence
        {
            return Err("concurrent helper request reached serialized TCP relay".into());
        }
        next_sequence += 1;
        match incoming.message {
            Some(Message::StreamData(data)) if data.channel == StreamChannel::Tcp as i32 => {
                if data.eof { tcp.eof()?; } else { tcp.send(data.data)?; }
            }
            Some(Message::Cancel(_)) => tcp.close()?,
            _ => return Err("invalid TCP relay control message".into()),
        }
    }
}

fn serve_format_only() -> Result<(), String> {
    let mut control = inherited_socket(CONTROL_FD)?;
    let workspace = inherited_file(WORKSPACE_FD, true)?;
    let envelope = read_frame(&mut control)
        .map_err(|error| error.to_string())?
        .ok_or("format request is required")?;
    if envelope.protocol_version != PROTOCOL_VERSION
        || envelope.request_id == 0
        || envelope.stream_id != 0
        || envelope.sequence != 0
    {
        return Err("format request envelope is invalid".into());
    }
    let Some(Message::FormatWorkspace(request)) = envelope.message.as_ref() else {
        return Err("format-workspace request is required".into());
    };
    let uuid: [u8; 16] = request
        .workspace_uuid
        .as_slice()
        .try_into()
        .map_err(|_| "Workspace UUID must contain 16 bytes")?;
    format_workspace(
        &workspace,
        request.logical_capacity_bytes,
        &request.label,
        uuid,
    )
    .map_err(|error| error.to_string())?;
    write_terminal(&mut control, &envelope, true, 0, "formatted")
}

fn supported_operations() -> Vec<protocol::Operation> {
    use protocol::Operation::*;
    vec![
        Start,
        Exec,
        FileRead,
        FileWrite,
        FileStat,
        FileList,
        FileMkdir,
        FileRemove,
        Pty,
        Tcp,
        Shutdown,
        FormatWorkspace,
    ]
}

fn write_terminal(
    control: &mut UnixStream,
    request: &Envelope,
    success: bool,
    exit_code: i32,
    reason: &str,
) -> Result<(), String> {
    write_frame(
        control,
        &Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: request.request_id,
            message: Some(Message::Terminal(TerminalEvent {
                success,
                exit_code,
                reason: bounded_diagnostic(reason),
                ..Default::default()
            })),
            ..Default::default()
        },
    )
    .map_err(|error| error.to_string())
}

fn write_exec_terminal(
    control: &mut UnixStream,
    request: &Envelope,
    success: bool,
    exit_code: i32,
    reason: &str,
    spawn_failure_reason: u32,
) -> Result<(), String> {
    write_frame(control, &Envelope {
        protocol_version: PROTOCOL_VERSION,
        request_id: request.request_id,
        stream_id: request.stream_id,
        message: Some(Message::Terminal(TerminalEvent {
            success, exit_code, reason: reason.into(), spawn_failure_reason,
            ..Default::default()
        })),
        ..Default::default()
    }).map_err(|error| error.to_string())
}

fn write_data(
    control: &mut UnixStream,
    request: &Envelope,
    channel: StreamChannel,
    data: Vec<u8>,
    eof: bool,
) -> Result<(), String> {
    write_frame(control, &Envelope {
        protocol_version: PROTOCOL_VERSION,
        request_id: request.request_id,
        stream_id: request.stream_id,
        message: Some(Message::StreamData(StreamData { data, eof, channel: channel as i32 })),
        ..Default::default()
    }).map_err(|error| error.to_string())
}

fn write_diagnostic(
    control: &mut UnixStream,
    request: &Envelope,
    code: &str,
    text: &str,
) -> Result<(), String> {
    write_frame(
        control,
        &Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: request.request_id,
            stream_id: request.stream_id,
            message: Some(Message::Diagnostic(DiagnosticEvent {
                code: code.into(),
                text: bounded_diagnostic(text),
            })),
            ..Default::default()
        },
    )
    .map_err(|error| error.to_string())
}

fn monitor_parent(shutting_down: Arc<AtomicBool>, workspace: File) -> Result<(), String> {
    let mut lifecycle = inherited_file(LIFECYCLE_FD, false)?;
    thread::Builder::new()
        .name("secondbox-parent-watchdog".into())
        .spawn(move || {
            let mut byte = [0_u8; 1];
            while lifecycle.read(&mut byte).is_ok_and(|count| count != 0) {}
            shutting_down.store(true, Ordering::Release);
            let (completed, flushed) = mpsc::sync_channel(1);
            if thread::Builder::new()
                .name("secondbox-parent-loss-flush".into())
                .spawn(move || {
                    let _ = completed.send(workspace.sync_all());
                })
                .is_err()
            {
                process::exit(1);
            }
            let status = match flushed.recv_timeout(PARENT_LOSS_FLUSH_BOUND) {
                Ok(Ok(())) => 0,
                Ok(Err(_)) | Err(_) => 1,
            };
            process::exit(status);
        })
        .map_err(|error| format!("start parent watchdog: {error}"))?;
    Ok(())
}

fn inherited_socket(fd: RawFd) -> Result<UnixStream, String> {
    if fd < 3 {
        return Err("inherited control descriptor is invalid".into());
    }
    // SAFETY: the runner transfers exclusive ownership of this fixed inherited descriptor.
    Ok(unsafe { UnixStream::from_raw_fd(fd) })
}

fn inherited_file(fd: RawFd, writable: bool) -> Result<File, String> {
    let path = format!("/proc/self/fd/{fd}");
    OpenOptions::new()
        .read(true)
        .write(writable)
        .open(path)
        .map_err(|error| format!("open inherited descriptor {fd}: {error}"))
}
