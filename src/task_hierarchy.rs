//! Display-only tree projection. The gateway's queue and ranks stay untouched.
use serde_json::{Value, json};
use std::collections::{HashMap, HashSet};

pub struct Row {
    pub task: Value,
    pub depth: usize,
    /// An ancestor retained to explain a matching descendant.
    pub context: bool,
    /// Parent metadata only: it may be in another project or outside the list window.
    pub stub: bool,
}

/// Preserve input order among siblings, retaining ancestors across view filters.
pub fn project(tasks: &[Value], matches: impl Fn(&Value) -> bool) -> Vec<Row> {
    let mut entries = tasks.to_vec();
    let mut indices = HashMap::new();
    for (index, task) in entries.iter().enumerate() {
        if let Some(id) = task["id"].as_str() {
            indices.insert(id.to_owned(), index);
        }
    }
    let complete = entries.len();
    for task in tasks {
        if let Some(id) = task["parent"]["id"].as_str()
            && !indices.contains_key(id)
        {
            indices.insert(id.to_owned(), entries.len());
            entries.push(json!({
                "id":id, "title":task["parent"]["title"],
                "project_ident":task["parent"]["project_ident"], "status":"parent"
            }));
        }
    }
    let selected = tasks.iter().map(&matches).collect::<Vec<_>>();
    let mut included = HashSet::new();
    for (index, selected) in selected.iter().enumerate() {
        if !selected {
            continue;
        }
        let mut current = index;
        while included.insert(current) {
            let Some(parent) = entries[current]["parent"]["id"]
                .as_str()
                .and_then(|id| indices.get(id))
                .copied()
            else {
                break;
            };
            current = parent;
        }
    }
    let mut children: HashMap<usize, Vec<usize>> = HashMap::new();
    let mut roots = Vec::new();
    for (index, task) in entries.iter().enumerate() {
        if !included.contains(&index) {
            continue;
        }
        match task["parent"]["id"]
            .as_str()
            .and_then(|id| indices.get(id))
            .copied()
        {
            Some(parent) if included.contains(&parent) && parent != index => {
                children.entry(parent).or_default().push(index);
            }
            _ => roots.push(index),
        }
    }
    let mut rows = Vec::new();
    let mut visited = HashSet::new();
    // The fallback also renders malformed legacy cycles once, without recursion.
    for root in roots.into_iter().chain(0..entries.len()) {
        if !included.contains(&root) {
            continue;
        }
        let mut stack = vec![(root, 0)];
        while let Some((index, depth)) = stack.pop() {
            if !visited.insert(index) {
                continue;
            }
            rows.push(Row {
                task: entries[index].clone(),
                depth,
                context: !selected.get(index).copied().unwrap_or(false),
                stub: index >= complete,
            });
            if let Some(children) = children.get(&index) {
                stack.extend(children.iter().rev().map(|child| (*child, depth + 1)));
            }
        }
    }
    rows
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn filters_retain_ancestors_and_siblings_keep_input_order() {
        let tasks = vec![
            json!({"id":"b", "parent":{"id":"a"}, "status":"todo"}),
            json!({"id":"c", "parent":{"id":"a"}, "status":"todo"}),
            json!({"id":"a", "status":"done"}),
            json!({"id":"other", "status":"done"}),
        ];
        let rows = project(&tasks, |task| task["status"] == "todo");
        assert_eq!(
            rows.iter()
                .map(|r| (r.task["id"].as_str().unwrap(), r.depth, r.context))
                .collect::<Vec<_>>(),
            [("a", 0, true), ("b", 1, false), ("c", 1, false)]
        );
        assert_eq!(tasks[0]["id"], "b");
    }

    #[test]
    fn missing_cross_project_parents_remain_visible_and_cycles_terminate() {
        let tasks = vec![
            json!({"id":"child", "parent":{"id":"remote", "title":"Feature", "project_ident":"other"}}),
            json!({"id":"a", "parent":{"id":"b"}}),
            json!({"id":"b", "parent":{"id":"a"}}),
        ];
        let rows = project(&tasks, |_| true);
        assert_eq!(rows.len(), 4);
        assert_eq!(rows[0].task["id"], "remote");
        assert!(rows[0].stub);
        assert_eq!(rows[1].depth, 1);
        assert_eq!(rows[0].task["project_ident"], "other");
    }
}
