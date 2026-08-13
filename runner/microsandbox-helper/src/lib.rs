//! Private inherited-descriptor protocol for one SecondBox Microsandbox Instance.

pub mod agent;
pub mod console;
pub mod frame;
pub mod state;
pub mod vmm;
pub mod workspace;

pub mod protocol {
    include!(concat!(
        env!("OUT_DIR"),
        "/secondbox.microsandbox.helper.v1.rs"
    ));
}

pub const PROTOCOL_VERSION: u32 = 1;
pub const HELPER_VERSION: &str = env!("CARGO_PKG_VERSION");
pub const MAX_DIAGNOSTIC_BYTES: usize = 4 * 1024;

pub fn bounded_diagnostic(text: &str) -> String {
    let mut end = text.len().min(MAX_DIAGNOSTIC_BYTES);
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].to_owned()
}
