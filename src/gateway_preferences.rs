//! Desktop connection preferences. Secrets are never included in Debug output.
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{
    io::{Read, Write},
    path::{Path, PathBuf},
};

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum GatewayMode {
    #[default]
    Legacy,
    Server,
    Client,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
#[serde(default)]
pub struct ServerPreferences {
    pub network_hosting: bool,
    pub server_name: String,
    pub secure_port: u16,
    pub discovery_port: u16,
}

impl Default for ServerPreferences {
    fn default() -> Self {
        Self {
            network_hosting: false,
            server_name: "localhost".into(),
            secure_port: 24182,
            discovery_port: 24181,
        }
    }
}

/// Public metadata and a reference to protected credentials, never a peering key.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct EnrolledConnection {
    pub url: String,
    pub cluster_id: String,
    pub node_id: String,
    pub profile_path: PathBuf,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(default)]
pub struct GatewayPreferences {
    pub mode: GatewayMode,
    pub server: ServerPreferences,
    pub enrolled: Option<EnrolledConnection>,
    pub url: String,
    pub api_key: String,
    pub automatic_notifications: bool,
    pub autoreview: bool,
    pub notify_exits: bool,
    pub auto_resume: bool,
    pub machine_id: String,
    pub machine_key: String,
}

impl Default for GatewayPreferences {
    fn default() -> Self {
        Self {
            mode: GatewayMode::Legacy,
            server: ServerPreferences::default(),
            enrolled: None,
            url: String::new(),
            api_key: String::new(),
            automatic_notifications: false,
            autoreview: false,
            notify_exits: false,
            auto_resume: true,
            machine_id: String::new(),
            machine_key: String::new(),
        }
    }
}

impl GatewayPreferences {
    pub fn from_environment() -> Self {
        let value = |name| std::env::var(name).unwrap_or_default();
        Self {
            url: value("AMUX_GATEWAY_URL"),
            api_key: value("AMUX_GATEWAY_KEY"),
            automatic_notifications: false,
            machine_id: value("AMUX_GATEWAY_MACHINE_ID"),
            machine_key: value("AMUX_GATEWAY_MACHINE_KEY"),
            ..Self::default()
        }
    }

    pub fn gateway(&self) -> Result<crate::gateway::Gateway> {
        crate::gateway::Gateway::from_profile(self.transport_profile()?)
    }

    pub fn has_connection(&self) -> bool {
        match self.mode {
            GatewayMode::Legacy => !self.url.trim().is_empty() && !self.api_key.trim().is_empty(),
            GatewayMode::Server | GatewayMode::Client => self.enrolled.is_some(),
        }
    }

    pub fn resolved_url(&self) -> Result<String> {
        match self.mode {
            GatewayMode::Legacy => Ok(self.url.trim().to_owned()),
            GatewayMode::Server | GatewayMode::Client => self
                .enrolled
                .as_ref()
                .map(|connection| connection.url.clone())
                .context("Connect to the server before using tasks"),
        }
    }

    pub fn authority_namespace(&self) -> Result<String> {
        Ok(self.transport_profile()?.namespace())
    }

    pub fn transport_profile(&self) -> Result<crate::gateway_transport::TransportProfile> {
        if self.mode != GatewayMode::Legacy {
            let connection = self
                .enrolled
                .as_ref()
                .context("Connect to the server before using tasks")?;
            return crate::gateway_transport::TransportProfile::enrolled(connection);
        }
        if self.url.trim().is_empty() || self.api_key.trim().is_empty() {
            bail!("Open Gateway preferences to enter a URL and API key");
        }
        self.api_key
            .parse::<reqwest::header::HeaderValue>()
            .map_err(|_| anyhow::anyhow!("API key contains invalid header characters"))?;
        crate::gateway_transport::TransportProfile::legacy(&self.url, self.api_key.clone())
    }

    pub fn validate(&self) -> Result<()> {
        if self.mode != GatewayMode::Legacy {
            if self.server.secure_port == 0
                || self.server.discovery_port == 0
                || self.server.secure_port == self.server.discovery_port
            {
                bail!("Choose different nonzero secure and discovery ports");
            }
            if self.enrolled.is_some() {
                self.transport_profile()?;
            }
        } else if self.automatic_notifications
            || self.autoreview
            || !self.url.trim().is_empty()
            || !self.api_key.trim().is_empty()
        {
            self.gateway()?;
        }
        if !self.machine_id.is_empty() || !self.machine_key.is_empty() {
            let id = uuid::Uuid::parse_str(&self.machine_id)
                .ok()
                .filter(|id| !id.is_nil() && id.to_string() == self.machine_id)
                .context("Enter the provisioned machine UUID and machine key together")?;
            crate::dispatch::Machine::new(id, self.machine_key.clone())
                .map_err(|_| anyhow::anyhow!("Invalid machine key"))?;
        }
        if serde_json::to_vec_pretty(self)?.len() > 32 * 1024 {
            bail!("Gateway preferences exceed 32 KiB");
        }
        Ok(())
    }

    pub fn load(path: &Path) -> Result<Option<Self>> {
        let file = match std::fs::File::open(path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(_) => bail!("Could not read saved gateway preferences"),
        };
        let mut bytes = Vec::new();
        file.take(32 * 1024 + 1).read_to_end(&mut bytes)?;
        if bytes.len() > 32 * 1024 {
            bail!("Saved gateway preferences exceed 32 KiB");
        }
        let preferences: Self = serde_json::from_slice(&bytes)
            .map_err(|_| anyhow::anyhow!("Saved gateway preferences are invalid"))?;
        preferences.validate()?;
        Ok(Some(preferences))
    }

    pub fn save(&self, path: &Path) -> Result<()> {
        self.validate()?;
        let directory = path.parent().context("Preferences path has no parent")?;
        std::fs::create_dir_all(directory)?;
        let mut file = tempfile::NamedTempFile::new_in(directory)?;
        file.write_all(&serde_json::to_vec_pretty(self)?)?;
        file.as_file().sync_all()?;
        file.persist(path).map_err(|error| error.error)?;
        Ok(())
    }
}
