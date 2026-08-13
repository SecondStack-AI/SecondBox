//! Bounded length-delimited protobuf framing.

use std::io::{self, Read, Write};

use prost::Message;
use thiserror::Error;

use crate::protocol::Envelope;

pub const MAX_FRAME_BYTES: usize = 1024 * 1024;

#[derive(Debug, Error)]
pub enum FrameError {
    #[error("SecondBox Microsandbox helper frame I/O: {0}")]
    Io(#[from] io::Error),
    #[error("SecondBox Microsandbox helper frame exceeds the {MAX_FRAME_BYTES}-byte bound")]
    Oversized,
    #[error("SecondBox Microsandbox helper frame is empty")]
    Empty,
    #[error("SecondBox Microsandbox helper frame is malformed: {0}")]
    Malformed(#[from] prost::DecodeError),
    #[error("SecondBox Microsandbox helper frame encoding failed: {0}")]
    Encode(#[from] prost::EncodeError),
}

pub fn read_frame(reader: &mut impl Read) -> Result<Option<Envelope>, FrameError> {
    let mut header = [0_u8; 4];
    match reader.read(&mut header[..1]) {
        Ok(0) => return Ok(None),
        Ok(1) => {}
        Ok(_) => unreachable!("one-byte header read returned more than one byte"),
        Err(error) => return Err(error.into()),
    }
    reader.read_exact(&mut header[1..])?;
    let length = u32::from_be_bytes(header) as usize;
    if length == 0 {
        return Err(FrameError::Empty);
    }
    if length > MAX_FRAME_BYTES {
        return Err(FrameError::Oversized);
    }
    let mut payload = vec![0; length];
    reader.read_exact(&mut payload)?;
    Ok(Some(Envelope::decode(payload.as_slice())?))
}

pub fn write_frame(writer: &mut impl Write, envelope: &Envelope) -> Result<(), FrameError> {
    let length = envelope.encoded_len();
    if length == 0 || length > MAX_FRAME_BYTES {
        return Err(FrameError::Oversized);
    }
    writer.write_all(&(length as u32).to_be_bytes())?;
    let mut payload = Vec::with_capacity(length);
    envelope.encode(&mut payload)?;
    writer.write_all(&payload)?;
    writer.flush()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::*;
    use crate::PROTOCOL_VERSION;

    proptest! {
        #[test]
        fn arbitrary_payloads_never_panic(payload in proptest::collection::vec(any::<u8>(), 0..4096)) {
            let _ = read_frame(&mut payload.as_slice());
        }
    }

    #[test]
    fn round_trip() {
        let want = Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: 7,
            ..Default::default()
        };
        let mut encoded = Vec::new();
        write_frame(&mut encoded, &want).unwrap();
        assert_eq!(read_frame(&mut encoded.as_slice()).unwrap(), Some(want));
    }

    #[test]
    fn oversized_header_is_rejected_without_allocation() {
        let encoded = ((MAX_FRAME_BYTES + 1) as u32).to_be_bytes();
        assert!(matches!(
            read_frame(&mut encoded.as_slice()),
            Err(FrameError::Oversized)
        ));
    }

    #[test]
    fn partial_header_is_rejected_as_io_failure() {
        assert!(matches!(
            read_frame(&mut [0_u8, 0_u8].as_slice()),
            Err(FrameError::Io(_))
        ));
    }
}
