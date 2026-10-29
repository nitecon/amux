//! Which machines have each project checked out. Desktops report their open
//! workspaces; each machine's last report is kept so offline machines still
//! show where a project lives. Display only: never used for routing.
use crate::{db, plugins::presence, routes::AppError, AppState};
use anyhow::Result;
use axum::{extract::State, Json};
use rusqlite::{params, Connection};
use serde::Deserialize;
use serde_json::{json, Value};

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS machine_checkouts (
            hostname      TEXT NOT NULL,
            project_ident TEXT NOT NULL,
            origin        TEXT,
            seen_at       INTEGER NOT NULL,
            PRIMARY KEY(hostname, project_ident)
        );",
    )?;
    Ok(())
}

#[derive(Deserialize)]
pub struct Checkout {
    pub project: String,
    pub origin: Option<String>,
}

#[derive(Deserialize)]
pub struct Report {
    pub hostname: String,
    pub checkouts: Vec<Checkout>,
}

/// Replaces the machine's previous report.
pub fn report(conn: &Connection, report: &Report) -> Result<()> {
    let hostname = report.hostname.trim();
    anyhow::ensure!(!hostname.is_empty(), "hostname is required");
    let tx = conn.unchecked_transaction()?;
    tx.execute(
        "DELETE FROM machine_checkouts WHERE hostname=?1",
        params![hostname],
    )?;
    let now = db::now_ms();
    for checkout in &report.checkouts {
        tx.execute(
            "INSERT OR REPLACE INTO machine_checkouts(hostname,project_ident,origin,seen_at) VALUES(?1,?2,?3,?4)",
            params![hostname, crate::projects::normalize_project_ident(&checkout.project), checkout.origin, now],
        )?;
    }
    tx.commit()?;
    Ok(())
}

/// Every project with the machines that have it checked out and its live agents.
pub fn overview(conn: &Connection) -> Result<Value> {
    let mut agents = std::collections::HashMap::<String, (usize, usize)>::new();
    for agent in presence::agents(conn, db::now_ms())? {
        if let Some(project) = agent.agent["project"].as_str() {
            let entry = agents.entry(project.to_owned()).or_default();
            entry.0 += 1;
            entry.1 += usize::from(agent.state == "busy");
        }
    }
    let mut machines = conn.prepare(
        "SELECT hostname, origin FROM machine_checkouts WHERE project_ident=?1 ORDER BY hostname",
    )?;
    db::list_project_stats(conn)?
        .into_iter()
        .map(|project| {
            let checkouts = machines
                .query_map(params![project.ident], |row| {
                    Ok(json!({"hostname": row.get::<_, String>(0)?,
                              "origin": row.get::<_, Option<String>>(1)?}))
                })?
                .collect::<rusqlite::Result<Vec<_>>>()?;
            let (count, active) = agents.get(&project.ident).copied().unwrap_or_default();
            Ok(
                json!({"ident": project.ident, "canonical_remote": project.canonical_remote,
                      "machines": checkouts, "agents": count, "active": active}),
            )
        })
        .collect::<Result<Vec<_>>>()
        .map(Value::from)
}

/// `PUT /v1/checkouts`.
pub async fn put(
    State(state): State<AppState>,
    Json(body): Json<Report>,
) -> std::result::Result<Json<Value>, AppError> {
    let conn = state.db.lock().unwrap();
    report(&conn, &body)?;
    Ok(Json(json!({"ok": true})))
}

/// `GET /v1/checkouts`.
pub async fn get(State(state): State<AppState>) -> std::result::Result<Json<Value>, AppError> {
    let conn = state.db.lock().unwrap();
    Ok(Json(overview(&conn)?))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn last_report_per_machine_is_kept_and_replaced() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        for ident in ["amux", "other"] {
            conn.execute(
                "INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES(?1,'none','',0)",
                [ident],
            )
            .unwrap();
        }
        let checkout = |project: &str| Checkout {
            project: project.into(),
            origin: Some(format!("git@github.com:nitecon/{project}.git")),
        };
        report(
            &conn,
            &Report {
                hostname: "Defcon2".into(),
                checkouts: vec![checkout("amux")],
            },
        )
        .unwrap();
        report(
            &conn,
            &Report {
                hostname: "Defcon1".into(),
                checkouts: vec![checkout("AMUX"), checkout("other")],
            },
        )
        .unwrap();
        let projects = overview(&conn).unwrap();
        let amux = projects
            .as_array()
            .unwrap()
            .iter()
            .find(|p| p["ident"] == "amux")
            .unwrap();
        assert_eq!(amux["machines"][0]["hostname"], "Defcon1");
        assert_eq!(amux["machines"][1]["hostname"], "Defcon2");
        assert_eq!(
            amux["machines"][1]["origin"],
            "git@github.com:nitecon/amux.git"
        );
        // A new report replaces the machine's old one rather than adding to it.
        report(
            &conn,
            &Report {
                hostname: "Defcon1".into(),
                checkouts: vec![],
            },
        )
        .unwrap();
        let projects = overview(&conn).unwrap();
        for project in projects.as_array().unwrap() {
            let hosts: Vec<_> = project["machines"]
                .as_array()
                .unwrap()
                .iter()
                .map(|m| m["hostname"].clone())
                .collect();
            assert!(!hosts.contains(&json!("Defcon1")), "{project}");
        }
        assert!(report(
            &conn,
            &Report {
                hostname: " ".into(),
                checkouts: vec![]
            }
        )
        .is_err());
    }
}
