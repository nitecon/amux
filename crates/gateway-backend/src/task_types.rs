//! Work classification is separate from normal/delegated task linkage.
use crate::task_dispatch::Conflict;
use anyhow::Result;

pub const GENERAL: &str = "general";
pub const SOFTWARE: &str = "software";

pub fn validate(value: &str) -> Result<()> {
    if !matches!(value, GENERAL | SOFTWARE) {
        return Err(Conflict("task_type must be general or software").into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, task_events, task_reviews};

    fn actor() -> task_events::AgentOrigin {
        task_events::AgentOrigin {
            session_id: uuid::Uuid::now_v7().to_string(),
            instance_id: uuid::Uuid::now_v7().to_string(),
            provider: "codex".into(),
            os: "linux".into(),
        }
    }

    #[test]
    fn legacy_classification_preserves_root_workflows_without_recursive_reviews() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('types','none','',0)", []).unwrap();
        let owner = actor();
        let other = actor();
        for software in [false, true] {
            let task = task_events::mutation(&conn, Some(&owner), |conn| {
                db::insert_task(conn, "types", "Work", None, None, &[], None, "fixture")
            })
            .unwrap();
            assert_eq!(task.task_type, GENERAL);
            let id = &task.id;
            let change_type = db::TaskUpdate {
                task_type: Some(SOFTWARE),
                ..Default::default()
            };
            assert!(
                task_events::mutation(&conn, Some(&other), |conn| db::update_task(
                    conn,
                    "types",
                    id,
                    &change_type,
                    Some(&other.session_id)
                ))
                .is_err()
            );
            task_events::mutation(&conn, Some(&owner), |conn| {
                db::update_task(
                    conn,
                    "types",
                    id,
                    &db::TaskUpdate {
                        status: Some("in_progress"),
                        ..Default::default()
                    },
                    Some(&owner.session_id),
                )
            })
            .unwrap();
            if software {
                let converted = task_events::mutation(&conn, Some(&owner), |conn| {
                    db::update_task(conn, "types", id, &change_type, Some(&owner.session_id))
                })
                .unwrap()
                .unwrap();
                assert_eq!(converted.task_type, SOFTWARE);
            }
            task_events::mutation(&conn, Some(&owner), |conn| {
                db::update_task(
                    conn,
                    "types",
                    id,
                    &db::TaskUpdate {
                        status: Some("done"),
                        ..Default::default()
                    },
                    Some(&owner.session_id),
                )
            })
            .unwrap();
            let review =
                task_events::mutation(&conn, None, |conn| task_reviews::enqueue(conn, "types", id))
                    .unwrap();
            assert_eq!(review.is_some(), software);
            if let Some(review) = review {
                assert_eq!(review.task_type, SOFTWARE);
                let downgrade = db::TaskUpdate {
                    task_type: Some(GENERAL),
                    ..Default::default()
                };
                assert!(db::update_task_from_user(&conn, "types", &review.id, &downgrade).is_err());
                task_events::mutation(&conn, None, |conn| {
                    db::update_task_from_user(
                        conn,
                        "types",
                        &review.id,
                        &db::TaskUpdate {
                            status: Some("done"),
                            ..Default::default()
                        },
                    )
                })
                .unwrap();
                assert!(
                    task_events::mutation(&conn, None, |conn| task_reviews::enqueue(
                        conn, "types", &review.id
                    ))
                    .unwrap()
                    .is_none()
                );
                let docs = crate::subtasks::list(&conn, id)
                    .unwrap()
                    .into_iter()
                    .find(|task| {
                        task.labels
                            .iter()
                            .any(|label| label == "documentation-review")
                    })
                    .unwrap();
                assert_eq!(docs.task_type, SOFTWARE);
                assert!(docs.labels.iter().any(|l| l == "documentation-review"));
            }
        }
        assert!(validate("code").is_err());
        let list = db::list_tasks(&conn, "types", &["done".into()], true).unwrap();
        assert!(list.iter().any(|t| t.task_type == GENERAL));
        assert!(list.iter().any(|t| t.task_type == SOFTWARE));
    }

    #[test]
    fn migration_preserves_existing_reviews_and_new_creation_defaults_to_general() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("old.sqlite");
        let database = db::open(path.to_str().unwrap()).unwrap();
        let id = {
            let conn = database.lock().unwrap();
            conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('types','none','',0)", []).unwrap();
            let task =
                db::insert_task(&conn, "types", "Old work", None, None, &[], None, "user").unwrap();
            let names: Vec<String> = conn.prepare("SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name IN ('tasks','task_comments')").unwrap().query_map([], |row| row.get(0)).unwrap().collect::<rusqlite::Result<_>>().unwrap();
            for name in names {
                conn.execute_batch(&format!("DROP TRIGGER \"{}\"", name.replace('"', "\"\"")))
                    .unwrap();
            }
            conn.execute_batch("ALTER TABLE tasks DROP COLUMN task_type")
                .unwrap();
            task.id
        };
        drop(database);
        let reopened = db::open(path.to_str().unwrap()).unwrap();
        let conn = reopened.lock().unwrap();
        assert_eq!(
            db::get_task_detail(&conn, "types", &id)
                .unwrap()
                .unwrap()
                .task
                .task_type,
            SOFTWARE
        );
        assert_eq!(
            db::insert_task(&conn, "types", "New work", None, None, &[], None, "user")
                .unwrap()
                .task_type,
            GENERAL
        );
    }
}
