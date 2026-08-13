//! Request and stream ordering independent of the VMM implementation.

use std::collections::{HashMap, HashSet};

use thiserror::Error;

use crate::{
    PROTOCOL_VERSION,
    protocol::{Envelope, envelope::Message},
};

#[derive(Debug, Error, Eq, PartialEq)]
pub enum StateError {
    #[error("SecondBox Microsandbox helper protocol version is unsupported")]
    Version,
    #[error("SecondBox Microsandbox helper request identity is invalid or duplicated")]
    Request,
    #[error("SecondBox Microsandbox helper stream sequence is stale or out of order")]
    Sequence,
    #[error("SecondBox Microsandbox helper stream credit is exhausted")]
    Credit,
    #[error("SecondBox Microsandbox helper frame kind is invalid")]
    Kind,
}

#[derive(Default)]
pub struct ProtocolState {
    requests: HashSet<u64>,
    streams: HashMap<u64, StreamState>,
}

#[derive(Default)]
struct StreamState {
    next_sequence: u64,
    credit: u64,
    eof: bool,
}

impl ProtocolState {
    pub fn admit(&mut self, envelope: &Envelope) -> Result<(), StateError> {
        if envelope.protocol_version != PROTOCOL_VERSION {
            return Err(StateError::Version);
        }
        let message = envelope.message.as_ref().ok_or(StateError::Kind)?;
        if envelope.request_id == 0 {
            return Err(StateError::Request);
        }
        if envelope.stream_id == 0 {
            if envelope.sequence != 0 || !self.requests.insert(envelope.request_id) {
                return Err(StateError::Request);
            }
            return Ok(());
        }
        if !self.requests.contains(&envelope.request_id) {
            return Err(StateError::Request);
        }
        let stream = self.streams.entry(envelope.stream_id).or_default();
        if envelope.sequence != stream.next_sequence || stream.eof {
            return Err(StateError::Sequence);
        }
        match message {
            Message::StreamCredit(credit) => {
                stream.credit = stream
                    .credit
                    .checked_add(credit.bytes)
                    .ok_or(StateError::Credit)?;
            }
            Message::StreamData(data) => {
                let bytes = u64::try_from(data.data.len()).map_err(|_| StateError::Credit)?;
                if bytes > stream.credit {
                    return Err(StateError::Credit);
                }
                stream.credit -= bytes;
                stream.eof = data.eof;
            }
            _ => return Err(StateError::Kind),
        }
        stream.next_sequence += 1;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::{ExecRequest, StreamCredit, StreamData};

    fn request() -> Envelope {
        Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: 1,
            message: Some(Message::Exec(ExecRequest::default())),
            ..Default::default()
        }
    }

    #[test]
    fn rejects_stale_unknown_and_uncredited_streams() {
        let mut state = ProtocolState::default();
        state.admit(&request()).unwrap();
        let credit = Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: 1,
            stream_id: 9,
            message: Some(Message::StreamCredit(StreamCredit { bytes: 3 })),
            ..Default::default()
        };
        state.admit(&credit).unwrap();
        assert_eq!(state.admit(&credit), Err(StateError::Sequence));
        let data = Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: 1,
            stream_id: 9,
            sequence: 1,
            message: Some(Message::StreamData(StreamData {
                data: vec![1, 2, 3, 4],
                eof: false,
            })),
        };
        assert_eq!(state.admit(&data), Err(StateError::Credit));
    }

    #[test]
    fn rejects_unknown_version_and_duplicate_request() {
        let mut state = ProtocolState::default();
        let mut message = request();
        message.protocol_version = 2;
        assert_eq!(state.admit(&message), Err(StateError::Version));
        message.protocol_version = PROTOCOL_VERSION;
        state.admit(&message).unwrap();
        assert_eq!(state.admit(&message), Err(StateError::Request));
    }
}
