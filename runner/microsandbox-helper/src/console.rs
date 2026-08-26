//! Bounded in-process virtio-console transport for the Microsandbox agent.

use std::{
    collections::VecDeque,
    io,
    os::fd::{AsRawFd, OwnedFd, RawFd},
    sync::{Arc, Mutex},
    time::Duration,
};

use msb_krun::ConsolePortBackend;

const QUEUE_CAPACITY: usize = 2048;

struct WakePipe {
    read: OwnedFd,
    write: OwnedFd,
}

impl WakePipe {
    fn new() -> io::Result<Self> {
        let (read, write) = crate::fd::pipe_cloexec(true)?;
        Ok(Self { read, write })
    }

    fn wake(&self) {
        let byte = [1_u8];
        // SAFETY: `write` remains valid for the lifetime of `self`; a full
        // nonblocking pipe already represents the required wakeup.
        unsafe {
            libc::write(self.write.as_raw_fd(), byte.as_ptr().cast(), 1);
        }
    }

    fn drain(&self) {
        let mut bytes = [0_u8; 256];
        loop {
            // SAFETY: `read` remains valid and `bytes` is writable.
            if unsafe {
                libc::read(
                    self.read.as_raw_fd(),
                    bytes.as_mut_ptr().cast(),
                    bytes.len(),
                )
            } <= 0
            {
                return;
            }
        }
    }

    fn wait(&self, timeout: Duration) -> io::Result<bool> {
        let millis = i32::try_from(timeout.as_millis()).unwrap_or(i32::MAX);
        let mut descriptor = libc::pollfd {
            fd: self.read.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: `descriptor` points to one initialized pollfd.
        let result = unsafe { libc::poll(&mut descriptor, 1, millis) };
        if result < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(result > 0)
    }
}

/// Shared, bounded byte queues named from the guest's perspective.
pub struct AgentConsole {
    guest_tx: Mutex<VecDeque<Vec<u8>>>,
    guest_rx: Mutex<VecDeque<Vec<u8>>>,
    guest_tx_wake: WakePipe,
    guest_rx_wake: WakePipe,
}

impl AgentConsole {
    pub fn new() -> io::Result<Arc<Self>> {
        Ok(Arc::new(Self {
            guest_tx: Mutex::new(VecDeque::with_capacity(QUEUE_CAPACITY)),
            guest_rx: Mutex::new(VecDeque::with_capacity(QUEUE_CAPACITY)),
            guest_tx_wake: WakePipe::new()?,
            guest_rx_wake: WakePipe::new()?,
        }))
    }

    pub fn backend(self: &Arc<Self>) -> AgentConsoleBackend {
        AgentConsoleBackend {
            shared: Arc::clone(self),
            pending: Mutex::new(VecDeque::new()),
        }
    }

    pub fn push_to_guest(&self, bytes: Vec<u8>) -> io::Result<()> {
        let mut queue = self.guest_rx.lock().expect("agent RX queue poisoned");
        if queue.len() == QUEUE_CAPACITY {
            return Err(io::Error::new(
                io::ErrorKind::WouldBlock,
                "SecondBox Microsandbox agent RX queue is full",
            ));
        }
        queue.push_back(bytes);
        drop(queue);
        self.guest_rx_wake.wake();
        Ok(())
    }

    pub fn take_from_guest(&self, timeout: Duration) -> io::Result<Option<Vec<u8>>> {
        self.guest_tx_wake.drain();
        if let Some(bytes) = self
            .guest_tx
            .lock()
            .expect("agent TX queue poisoned")
            .pop_front()
        {
            return Ok(Some(bytes));
        }
        if !self.guest_tx_wake.wait(timeout)? {
            return Ok(None);
        }
        self.guest_tx_wake.drain();
        Ok(self
            .guest_tx
            .lock()
            .expect("agent TX queue poisoned")
            .pop_front())
    }
}

pub struct AgentConsoleBackend {
    shared: Arc<AgentConsole>,
    pending: Mutex<VecDeque<u8>>,
}

impl ConsolePortBackend for AgentConsoleBackend {
    fn read(&self, output: &mut [u8]) -> io::Result<usize> {
        self.shared.guest_rx_wake.drain();
        let mut pending = self.pending.lock().expect("agent console pending poisoned");
        if pending.is_empty() {
            let Some(bytes) = self
                .shared
                .guest_rx
                .lock()
                .expect("agent RX queue poisoned")
                .pop_front()
            else {
                return Err(io::ErrorKind::WouldBlock.into());
            };
            pending.extend(bytes);
        }
        let count = output.len().min(pending.len());
        for destination in &mut output[..count] {
            *destination = pending.pop_front().expect("pending length changed");
        }
        Ok(count)
    }

    fn write(&self, input: &[u8]) -> io::Result<usize> {
        let mut queue = self
            .shared
            .guest_tx
            .lock()
            .expect("agent TX queue poisoned");
        if queue.len() == QUEUE_CAPACITY {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        queue.push_back(input.to_vec());
        drop(queue);
        self.shared.guest_tx_wake.wake();
        Ok(input.len())
    }

    fn read_wake_fd(&self) -> RawFd {
        self.shared.guest_rx_wake.read.as_raw_fd()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn transports_bytes_in_both_directions() {
        let shared = AgentConsole::new().unwrap();
        let backend = shared.backend();
        assert_eq!(backend.write(b"guest").unwrap(), 5);
        assert_eq!(
            shared.take_from_guest(Duration::from_millis(1)).unwrap(),
            Some(b"guest".to_vec())
        );
        shared.push_to_guest(b"host".to_vec()).unwrap();
        let mut output = [0_u8; 4];
        assert_eq!(backend.read(&mut output).unwrap(), 4);
        assert_eq!(&output, b"host");
    }
}
