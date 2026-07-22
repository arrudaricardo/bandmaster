//! Shared types for the Rust implementation of Bandmaster.
//!
//! The port deliberately keeps persisted state values such as statuses and
//! timestamps as strings. This preserves the Go implementation's JSON
//! contract and lets the command layer validate transitions in one place.

pub mod error;
pub mod model;

pub use error::{BandmasterError, ErrorDetail};
pub use model::*;
