use serde::{Deserialize, Serialize};
use thiserror::Error;

/// Stable error payload returned by the command-line interface.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ErrorDetail {
    pub code: String,
    pub message: String,
    #[serde(default)]
    pub retryable: bool,
}

impl ErrorDetail {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            retryable: false,
        }
    }
}

/// Errors used inside the Rust implementation.
#[derive(Debug, Error)]
pub enum BandmasterError {
    #[error("{0}")]
    Invalid(String),
    #[error("state error: {0}")]
    State(#[from] rusqlite::Error),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
    #[error("configuration error: {0}")]
    Configuration(#[from] serde_yaml::Error),
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
}

impl BandmasterError {
    pub fn detail(&self) -> ErrorDetail {
        match self {
            Self::Invalid(message) => ErrorDetail::new("invalid_arguments", message),
            Self::State(_) => ErrorDetail::new("state_error", self.to_string()),
            Self::Json(_) => ErrorDetail::new("invalid_json", self.to_string()),
            Self::Configuration(_) => ErrorDetail::new("invalid_configuration", self.to_string()),
            Self::Io(_) => ErrorDetail::new("io_error", self.to_string()),
        }
    }
}
