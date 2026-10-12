//! Domain model for the "My issues" view. The JSON fixture in `data/my_issues.json` is generated
//! by `tools/build_assets.mjs` from a live extraction; later this is where a GraphQL/sync layer
//! would plug in.

use gpui::{Hsla, SharedString};
use serde::{Deserialize, Deserializer};

use ui::theme::parse_css_color;

fn color<'de, D: Deserializer<'de>>(d: D) -> Result<Hsla, D::Error> {
    let s = String::deserialize(d)?;
    parse_css_color(&s).ok_or_else(|| serde::de::Error::custom(format!("bad color {s}")))
}

#[derive(Deserialize)]
pub struct Fixture {
    pub workspace: Workspace,
    pub user: User,
    pub labels: Vec<Label>,
    pub projects: Vec<Project>,
    pub favorites: Vec<Favorite>,
    pub teams: Vec<Team>,
    pub issues: Vec<Issue>,
}

impl Fixture {
    pub fn load() -> anyhow::Result<Self> {
        Ok(serde_json::from_slice(include_bytes!("../data/my_issues.json"))?)
    }

    pub fn project(&self, name: &str) -> Option<&Project> {
        self.projects.iter().find(|p| p.name == name)
    }

    pub fn label(&self, name: &str) -> Option<&Label> {
        self.labels.iter().find(|l| l.name == name)
    }
}

#[derive(Deserialize)]
pub struct Workspace {
    pub name: SharedString,
}

#[derive(Deserialize)]
pub struct User {
    pub name: SharedString,
    pub avatar: SharedString,
}

#[derive(Deserialize)]
pub struct Label {
    pub name: SharedString,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
}

#[derive(Deserialize)]
pub struct Project {
    pub name: SharedString,
    pub icon: SharedString,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
}

#[derive(Deserialize)]
pub struct Team {
    pub name: SharedString,
    pub key: SharedString,
    pub icon: SharedString,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
}

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Favorite {
    Document {
        title: SharedString,
        subtitle: SharedString,
        icon: SharedString,
        #[serde(deserialize_with = "color")]
        color: Hsla,
    },
    View {
        title: SharedString,
        icon: SharedString,
    },
    Folder {
        title: SharedString,
        children: Vec<Favorite>,
    },
}

#[derive(Deserialize, Clone, Copy, PartialEq, Eq, Debug)]
#[serde(rename_all = "snake_case")]
pub enum Priority {
    Urgent,
    High,
    Medium,
    Low,
    None,
}

#[derive(Deserialize, Clone, Copy, PartialEq, Eq, Debug)]
#[serde(rename_all = "snake_case")]
pub enum StatusKind {
    Backlog,
    Unstarted,
    Started,
    Completed,
    Canceled,
}

#[derive(Deserialize, Clone)]
pub struct Status {
    pub name: SharedString,
    pub kind: StatusKind,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
    /// 0..=1 – Linear fills the inner pie of "started" states proportionally to workflow position
    pub progress: f32,
}

#[derive(Deserialize, Clone, Copy, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum DueUrgency {
    Overdue,
    Today,
    Soon,
    Normal,
}

#[derive(Deserialize, Clone)]
pub struct Due {
    pub label: SharedString,
    pub urgency: DueUrgency,
}

#[derive(Deserialize, Clone, Copy)]
pub struct SubIssues {
    pub done: u32,
    pub total: u32,
}

#[derive(Deserialize, Clone)]
pub struct Issue {
    pub identifier: SharedString,
    pub title: SharedString,
    pub priority: Priority,
    pub status: Status,
    pub project: Option<SharedString>,
    pub labels: Vec<SharedString>,
    pub due: Option<Due>,
    pub blocked_by: u32,
    pub blocking: u32,
    pub sub_issues: Option<SubIssues>,
    pub assignee: Option<SharedString>,
    pub created: SharedString,
}

/// The sections of "My issues › Assigned", in display order.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Hash)]
pub enum GroupKind {
    Urgent,
    Blocking,
    OtherActive,
    Backlog,
    Completed,
    Canceled,
}

impl GroupKind {
    pub const ALL: [GroupKind; 6] = [
        GroupKind::Urgent,
        GroupKind::Blocking,
        GroupKind::OtherActive,
        GroupKind::Backlog,
        GroupKind::Completed,
        GroupKind::Canceled,
    ];

    pub fn title(self) -> &'static str {
        match self {
            GroupKind::Urgent => "Urgent issues",
            GroupKind::Blocking => "Blocking issues",
            GroupKind::OtherActive => "Other active",
            GroupKind::Backlog => "Backlog",
            GroupKind::Completed => "Completed",
            GroupKind::Canceled => "Canceled",
        }
    }

    /// Only active sections offer the inline "create issue" affordance.
    pub fn can_create(self) -> bool {
        matches!(self, GroupKind::Urgent | GroupKind::OtherActive)
    }

    /// Which section an issue lands in. Every issue belongs to exactly one group, so the order of
    /// the checks encodes precedence when several criteria apply at once.
    pub fn for_issue(issue: &Issue) -> GroupKind {
        // closed states win over everything, then urgency, then "is blocking someone" – which
        // even pulls backlog issues up (HAB-71)
        match issue.status.kind {
            StatusKind::Completed => GroupKind::Completed,
            StatusKind::Canceled => GroupKind::Canceled,
            _ if issue.priority == Priority::Urgent => GroupKind::Urgent,
            _ if issue.blocking > 0 => GroupKind::Blocking,
            StatusKind::Backlog => GroupKind::Backlog,
            StatusKind::Unstarted | StatusKind::Started => GroupKind::OtherActive,
        }
    }
}

pub struct Group {
    pub kind: GroupKind,
    pub issues: Vec<usize>,
}

/// Buckets issues into sections, keeping the fixture order inside each section and dropping
/// empty sections (Linear hides them too).
pub fn group_issues(issues: &[Issue]) -> Vec<Group> {
    GroupKind::ALL
        .iter()
        .map(|&kind| Group {
            kind,
            issues: (0..issues.len()).filter(|&ix| GroupKind::for_issue(&issues[ix]) == kind).collect(),
        })
        .filter(|g| !g.issues.is_empty())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn groups_match_linear_reference() {
        let fixture = Fixture::load().unwrap();
        let counts: Vec<_> = group_issues(&fixture.issues).iter().map(|g| (g.kind, g.issues.len())).collect();
        assert_eq!(
            counts,
            [
                (GroupKind::Urgent, 2),
                (GroupKind::Blocking, 3),
                (GroupKind::OtherActive, 11),
                (GroupKind::Backlog, 11),
                (GroupKind::Completed, 4),
                (GroupKind::Canceled, 1),
            ]
        );
    }
}
