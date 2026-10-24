//! Desktop connection preferences. Secrets are never included in Debug output.
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{
    io::{Read, Write},
    path::Path,
};

#[derive(Clone, Deserialize, Serialize)]
#[serde(default)]
pub struct GatewayPreferences {
    pub url: String,
    pub api_key: String,
    pub automatic_notifications: bool,
    pub notify_exits: bool,
    pub auto_resume: bool,
    pub machine_id: String,
    pub machine_key: String,
}

impl Default for GatewayPreferences {
    fn default() -> Self {
        Self {
            url: String::new(),
            api_key: String::new(),
            automatic_notifications: false,
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
        if self.url.trim().is_empty() || self.api_key.trim().is_empty() {
            bail!("Open Gateway preferences to enter a URL and API key");
        }
        self.api_key
            .parse::<reqwest::header::HeaderValue>()
            .map_err(|_| anyhow::anyhow!("API key contains invalid header characters"))?;
        crate::gateway::Gateway::new(self.url.trim(), self.api_key.clone())
    }

    pub fn validate(&self) -> Result<()> {
        if self.automatic_notifications
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
