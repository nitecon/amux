//! Installation model catalog and user-selected launch/delegation profiles.
use crate::identity::Provider;
use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    io::Read,
    path::{Path, PathBuf},
};

pub const CATALOG_FILE: &str = "models.json";

#[derive(Debug, Clone, Deserialize)]
pub struct Model {
    pub provider: Provider,
    pub id: String,
    pub label: String,
    pub thinking: Vec<String>,
}
#[derive(Debug, Clone, Deserialize)]
pub struct Catalog {
    pub version: u32,
    pub models: Vec<Model>,
}
impl Catalog {
    pub fn path() -> Result<PathBuf> {
        let exe = std::env::current_exe()?;
        let beside = exe
            .parent()
            .context("executable has no directory")?
            .join(CATALOG_FILE);
        if beside.exists() {
            return Ok(beside);
        }
        // macOS keeps data in Resources, inside the sealed application.
        #[cfg(target_os = "macos")]
        if let Some(path) = exe
            .parent()
            .and_then(Path::parent)
            .map(|p| p.join("Resources").join(CATALOG_FILE))
            .filter(|p| p.exists())
        {
            return Ok(path);
        }
        #[cfg(debug_assertions)]
        {
            Ok(Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("assets")
                .join(CATALOG_FILE))
        }
        #[cfg(not(debug_assertions))]
        Ok(beside)
    }
    pub fn load() -> Result<Self> {
        Self::read(&Self::path()?)
    }
    pub fn read(path: &Path) -> Result<Self> {
        let mut bytes = Vec::new();
        std::fs::File::open(path)
            .with_context(|| format!("Read model catalog {}", path.display()))?
            .take(64 * 1024 + 1)
            .read_to_end(&mut bytes)?;
        ensure!(bytes.len() <= 64 * 1024, "Model catalog exceeds 64 KiB");
        let catalog: Self = serde_json::from_slice(&bytes).context("Invalid model catalog JSON")?;
        ensure!(
            catalog.version == 1 && !catalog.models.is_empty() && catalog.models.len() <= 256,
            "Unsupported or empty model catalog"
        );
        for (i, model) in catalog.models.iter().enumerate() {
            ensure!(
                token(&model.id) && !model.label.is_empty() && model.label.len() <= 80,
                "Invalid model entry"
            );
            ensure!(
                model.thinking.iter().all(|s| token(s)),
                "Invalid thinking level"
            );
            ensure!(
                !catalog.models[..i]
                    .iter()
                    .any(|m| m.provider == model.provider && m.id == model.id),
                "Duplicate model entry"
            );
        }
        Ok(catalog)
    }
    pub fn validate(&self, provider: Provider, profile: &Profile) -> Result<()> {
        if profile.model.is_empty() {
            ensure!(
                profile.thinking.is_empty(),
                "Choose a model before its thinking level"
            );
            return Ok(());
        }
        let model = self
            .models
            .iter()
            .find(|m| m.provider == provider && m.id == profile.model)
            .context("Selected model is absent from this provider's catalog")?;
        ensure!(
            profile.thinking.is_empty() || model.thinking.contains(&profile.thinking),
            "Thinking level is unsupported for the selected model"
        );
        Ok(())
    }
}
fn token(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 128
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-._:/".contains(&b))
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Profile {
    pub model: String,
    pub thinking: String,
}
impl Profile {
    pub fn new(model: &str, thinking: &str) -> Self {
        Self {
            model: model.into(),
            thinking: thinking.into(),
        }
    }
    pub fn arguments(&self, provider: Provider) -> Vec<String> {
        let mut args = Vec::new();
        if !self.model.is_empty() {
            args.extend(["--model".into(), self.model.clone()]);
        }
        if !self.thinking.is_empty() {
            match provider {
                Provider::Codex => args.extend([
                    "-c".into(),
                    format!("model_reasoning_effort={}", self.thinking),
                ]),
                Provider::Claude => args.extend(["--effort".into(), self.thinking.clone()]),
            }
        }
        args
    }
    fn description(&self) -> String {
        if self.thinking.is_empty() {
            self.model.clone()
        } else {
            format!("{} ({} thinking)", self.model, self.thinking)
        }
    }
}
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ProviderProfiles {
    pub launch: Profile,
    pub code: Profile,
    pub basic: Profile,
    pub complex: Profile,
}
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Preferences {
    pub codex: ProviderProfiles,
    pub claude: ProviderProfiles,
    /// Terminal numbers are allocated globally on this installation, not per project.
    pub terminals: BTreeMap<String, Profile>,
}
impl Default for Preferences {
    fn default() -> Self {
        Self {
            codex: ProviderProfiles {
                launch: Profile::default(),
                code: Profile::new("gpt-6-luna", "medium"),
                basic: Profile::new("gpt-6.1-terra", "medium"),
                complex: Profile::new("gpt-6.1-sol", "medium"),
            },
            claude: ProviderProfiles {
                launch: Profile::default(),
                code: Profile::new("sonnet", "medium"),
                basic: Profile::new("haiku", ""),
                complex: Profile::new("opus", "medium"),
            },
            terminals: BTreeMap::new(),
        }
    }
}
impl Preferences {
    pub fn provider(&self, p: Provider) -> &ProviderProfiles {
        match p {
            Provider::Codex => &self.codex,
            Provider::Claude => &self.claude,
        }
    }
    pub fn provider_mut(&mut self, p: Provider) -> &mut ProviderProfiles {
        match p {
            Provider::Codex => &mut self.codex,
            Provider::Claude => &mut self.claude,
        }
    }
    pub fn launch(&self, p: Provider, terminal: u64) -> &Profile {
        self.terminals
            .get(&format!("{}:{terminal}", p.command()))
            .unwrap_or(&self.provider(p).launch)
    }
    pub fn validate(&self, catalog: &Catalog) -> Result<()> {
        for p in [Provider::Codex, Provider::Claude] {
            let profiles = self.provider(p);
            catalog.validate(p, &profiles.launch)?;
            for profile in [&profiles.code, &profiles.basic, &profiles.complex] {
                ensure!(
                    !profile.model.is_empty(),
                    "Choose a model for each delegation category"
                );
                catalog.validate(p, profile)?;
            }
        }
        for (key, profile) in &self.terminals {
            let (provider, number) = key
                .split_once(':')
                .context("Invalid terminal profile key")?;
            ensure!(
                number.parse::<u64>().is_ok_and(|n| n > 0),
                "Invalid terminal number"
            );
            let provider = match provider {
                "codex" => Provider::Codex,
                "claude" => Provider::Claude,
                _ => anyhow::bail!("Invalid terminal provider"),
            };
            catalog.validate(provider, profile)?;
        }
        Ok(())
    }
    pub fn guidance(&self, p: Provider) -> String {
        let profiles = self.provider(p);
        format!(
            "[Amux model routing — current Settings] Explicitly use provider-native subagents first for this task. Routine code changes: {}; research/basic tasks: {}; complex work (including complex code/research): {}. Complexity overrides routine defaults. You, the lead, retain Amux task ownership, scope the delegation, review results and submit/complete the task yourself; helpers must not claim, release, complete or submit Amux tasks. Use a fresh subagent context when selecting another model; verify its effective model. If delegation or a configured model is unavailable, report the limitation rather than silently substituting. Keep answering the user while helpers run where the provider supports it. Review tasks remain reviews: delegate analysis only and do not implement findings. Updates/comments are context, not new assignments. These are the current saved settings and supersede earlier routing preferences.",
            profiles.code.description(),
            profiles.basic.description(),
            profiles.complex.description()
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn catalog_rejects_wrong_provider_and_unsupported_effort() {
        let catalog = Catalog::read(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("assets/models.json")
                .as_path(),
        )
        .unwrap();
        Preferences::default().validate(&catalog).unwrap();
        assert!(
            catalog
                .validate(Provider::Codex, &Profile::new("sonnet", "medium"))
                .is_err()
        );
        assert!(
            catalog
                .validate(Provider::Claude, &Profile::new("haiku", "medium"))
                .is_err()
        );
        assert_eq!(
            Profile::new("gpt-6-luna", "medium").arguments(Provider::Codex),
            [
                "--model",
                "gpt-6-luna",
                "-c",
                "model_reasoning_effort=medium"
            ]
        );
    }
    #[test]
    fn saved_profiles_and_current_policy_are_independent() {
        let mut settings = Preferences::default();
        settings
            .terminals
            .insert("codex:7".into(), Profile::new("gpt-6.1-sol", "high"));
        let saved: Preferences =
            serde_json::from_slice(&serde_json::to_vec(&settings).unwrap()).unwrap();
        assert_eq!(saved.launch(Provider::Codex, 7).thinking, "high");
        assert!(saved.launch(Provider::Codex, 8).model.is_empty());
        assert!(saved.launch(Provider::Claude, 7).model.is_empty());
        let before = settings.guidance(Provider::Codex);
        assert!(
            before.contains("gpt-6-luna")
                && before.contains("gpt-6.1-terra")
                && before.contains("gpt-6.1-sol")
        );
        settings.codex.code = Profile::new("gpt-6.1-terra", "low");
        assert!(!settings.guidance(Provider::Codex).contains("gpt-6-luna"));
        assert_eq!(settings.launch(Provider::Codex, 7).thinking, "high");
        let claude = settings.guidance(Provider::Claude);
        assert!(claude.contains("sonnet") && claude.contains("haiku") && claude.contains("opus"));
        assert!(!claude.contains("gpt-"));
    }
}
