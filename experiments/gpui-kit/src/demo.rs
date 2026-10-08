//! Deterministic mail used by the native Durian reference UI.

use crate::data::{Message, Thread, ThreadPreview};

pub const OWN_EMAIL: &str = "julian@js-lab.org";

const OWN: &str = "Julian <julian@js-lab.org>";

struct DemoMessage {
    from: &'static str,
    to: &'static str,
    date: &'static str,
    body: &'static str,
}

fn message(
    from: &'static str,
    to: &'static str,
    date: &'static str,
    body: &'static str,
) -> DemoMessage {
    DemoMessage {
        from,
        to,
        date,
        body,
    }
}

fn first_line(body: &str) -> String {
    body.lines()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .unwrap_or("")
        .to_string()
}

fn conversation(
    thread_id: &str,
    subject: &str,
    sender: &str,
    date: &str,
    tags: &[&str],
    unread: bool,
    messages: Vec<DemoMessage>,
) -> (ThreadPreview, Thread) {
    debug_assert!(!messages.is_empty());
    let preview = first_line(messages.first().map_or("", |message| message.body));
    let message_count = messages.len();
    let messages = messages
        .into_iter()
        .map(|message| Message {
            from: message.from.to_string(),
            to: message.to.to_string(),
            date: message.date.to_string(),
            body: message.body.to_string(),
            ..Message::default()
        })
        .collect();

    (
        ThreadPreview {
            thread_id: thread_id.to_string(),
            subject: subject.to_string(),
            sender: sender.to_string(),
            preview,
            date: date.to_string(),
            tags: tags.iter().map(|tag| (*tag).to_string()).collect(),
            unread,
            message_count,
        },
        Thread {
            subject: subject.to_string(),
            messages,
        },
    )
}

/// Returns stable, newest-first conversations for every default Durian folder.
pub fn mail() -> Vec<(ThreadPreview, Thread)> {
    let mut mail = vec![
        conversation(
            "team-offsite",
            "Team Offsite: March 28 — Munich",
            "Lisa Wang, Team Operations",
            "Mar 22",
            &["inbox", "flagged", "events"],
            false,
            vec![message(
                "Lisa Wang <lisa@northstar.example>",
                "Team Operations <all-hands@northstar.example>",
                "Sun, 22 Mar 2026 09:18:00 +0100",
                "Hi everyone, you’re invited to our Q2 team offsite in Munich on March 28.\n\nWe’ll start at 09:30 with the strategy review, break into product planning groups after lunch, and finish with dinner near the Isar. Please send dietary requirements to Team Operations by Tuesday.\n\nSee you there,\nLisa",
            )],
        ),
        conversation(
            "q1-revenue",
            "Q1 Revenue Report",
            "Sarah Chen, Finance Team",
            "Mar 22",
            &["inbox", "unread", "finance"],
            true,
            vec![message(
                "Sarah Chen <sarah@northstar.example>",
                OWN_EMAIL,
                "Sun, 22 Mar 2026 08:42:00 +0100",
                "The Q1 revenue report is ready. Revenue is up 23% year over year, led by enterprise renewals in Europe.\n\nThe workbook includes the regional breakdown, forecast variance, and the three assumptions we should revisit before Tuesday’s planning session.\n\nSarah",
            )],
        ),
        conversation(
            "design-review",
            "Design Review: New Dashboard",
            "Alex Rivera, Julian",
            "Mar 22",
            &["inbox", "design"],
            false,
            vec![
                message(
                    "Alex Rivera <alex@northstar.example>",
                    OWN_EMAIL,
                    "Sun, 22 Mar 2026 10:33:00 +0100",
                    "The revised dashboard mockups are ready for review.\n\nI kept the card-based overview, reduced the number of accent colors, and moved the date range next to the report title. The charts now share one scale, so comparing revenue and retention should be much quicker.\n\nCould we take 30 minutes on Tuesday to walk through the empty and loading states?\n\nCheers,\nAlex",
                ),
                message(
                    OWN,
                    "Alex Rivera <alex@northstar.example>",
                    "Sat, 21 Mar 2026 16:05:00 +0100",
                    "The direction looks good. The quieter summary cards make the trend chart much easier to find.\n\nBefore the review, could you try the filter with a long account name and add a comparison for the no-data state? Tuesday at 14:00 works for me.\n\nJulian",
                ),
                message(
                    "Alex Rivera <alex@northstar.example>",
                    OWN_EMAIL,
                    "Fri, 20 Mar 2026 11:12:00 +0100",
                    "I have a first pass at the dashboard redesign.\n\nThe layout follows the hierarchy from last week’s workshop: key totals first, trends second, and account detail only when it helps explain a change. I’d value your read on density before I polish the interaction states.\n\nAlex",
                ),
            ],
        ),
        conversation(
            "api-migration",
            "Re: API Migration Timeline",
            "James Park, Julian, Lisa Wang",
            "Mar 22",
            &["inbox", "engineering"],
            false,
            vec![
                message(
                    "James Park <james@northstar.example>",
                    "Julian <julian@js-lab.org>, Lisa Wang <lisa@northstar.example>",
                    "Sun, 22 Mar 2026 09:48:00 +0100",
                    "Good call — I added a rollback section with step-by-step instructions.\n\nThe runbook now separates a routine rollback from a partial regional failure. For a routine rollback, on-call disables the v2 write flag, waits for the queue depth to settle, and restores v1 reads only after the comparison dashboard is green for ten minutes. A regional failure keeps v2 reads enabled elsewhere and drains only the affected workers, which avoids turning one bad deployment into a global event.\n\nI also documented the checks that used to live in chat: compare accepted-event counts, inspect the dead-letter queue, sample five migrated accounts, and confirm that webhook retries remain below the alert threshold. Each check names its dashboard and the person responsible, so the handoff does not depend on whoever wrote the deploy plan being online.\n\nThe database section now calls out the one-way address normalization. We will leave the compatibility column in place through the April release and backfill in small batches. If error rate rises above 0.5%, the job pauses automatically; rollback does not remove data already normalized, but v1 can continue reading it safely.\n\nFor Tuesday’s rehearsal I suggest we use the staging tenant with the largest event history. Lisa can run the customer-facing checklist, I’ll drive the deploy, and Julian can time the rollback from the first alert. I expect the full exercise to take about 45 minutes, including the cache warm-up.\n\nIf you agree, I’ll freeze this version Monday afternoon and link it from the on-call handover.\n\nJames",
                ),
                message(
                    OWN,
                    "James Park <james@northstar.example>, Lisa Wang <lisa@northstar.example>",
                    "Sat, 21 Mar 2026 17:26:00 +0100",
                    "The cutover sequence is clear now. Please add the exact rollback triggers and note which data changes are safe for v1 to read; that is the part the on-call engineer will need under pressure.\n\nJulian",
                ),
                message(
                    "Lisa Wang <lisa@northstar.example>",
                    "James Park <james@northstar.example>, Julian <julian@js-lab.org>",
                    "Fri, 20 Mar 2026 15:40:00 +0100",
                    "Support has signed off on the customer notice. We can publish it only if the maintenance window extends past 20 minutes, and the status-page copy is ready for both delay and rollback cases.\n\nLisa",
                ),
                message(
                    "James Park <james@northstar.example>",
                    "Julian <julian@js-lab.org>, Lisa Wang <lisa@northstar.example>",
                    "Thu, 19 Mar 2026 13:08:00 +0100",
                    "Retry behavior is fixed in the migration worker. A restarted job resumes from its last acknowledged page instead of replaying the whole account, and duplicate events are discarded by the existing idempotency key.\n\nJames",
                ),
                message(
                    OWN,
                    "James Park <james@northstar.example>",
                    "Wed, 18 Mar 2026 18:14:00 +0100",
                    "The new latency panels look right. Could we keep separate lines for migrated and unmigrated tenants during the ramp? The aggregate hides the cache penalty in the first ten minutes.\n\nJulian",
                ),
                message(
                    "Lisa Wang <lisa@northstar.example>",
                    "Julian <julian@js-lab.org>, James Park <james@northstar.example>",
                    "Tue, 17 Mar 2026 10:22:00 +0100",
                    "The SDK owners confirmed that all supported clients tolerate the new response field. Android 6.8 is the oldest version in the sample, and it ignores the field as expected.\n\nLisa",
                ),
                message(
                    "James Park <james@northstar.example>",
                    "Julian <julian@js-lab.org>",
                    "Mon, 16 Mar 2026 16:51:00 +0100",
                    "I split the migration into 5%, 20%, 50%, and 100% cohorts. Each gate requires one business day without elevated errors, except the final step, which can happen after the Wednesday review.\n\nJames",
                ),
                message(
                    OWN,
                    "James Park <james@northstar.example>, Lisa Wang <lisa@northstar.example>",
                    "Sun, 15 Mar 2026 12:07:00 +0100",
                    "Let’s avoid a Friday cutover. If the 50% cohort is healthy by Thursday, hold it there through the weekend and schedule the final ramp for Monday morning.\n\nJulian",
                ),
                message(
                    "Lisa Wang <lisa@northstar.example>",
                    "Julian <julian@js-lab.org>, James Park <james@northstar.example>",
                    "Sat, 14 Mar 2026 09:35:00 +0100",
                    "I reviewed the account list with Customer Success. Two customers have quarter-end imports that week, so I moved them to the last cohort and added their owners to the notification sheet.\n\nLisa",
                ),
                message(
                    "James Park <james@northstar.example>",
                    "Julian <julian@js-lab.org>",
                    "Fri, 13 Mar 2026 14:18:00 +0100",
                    "The shadow-read comparison finished overnight: 99.98% of responses matched. The remaining cases are timestamp formatting differences, not missing records; I opened a small fix for the serializer.\n\nJames",
                ),
                message(
                    OWN,
                    "James Park <james@northstar.example>",
                    "Thu, 12 Mar 2026 11:46:00 +0100",
                    "Please include p95 and p99 response times in the comparison. The median is stable, but batch export is sensitive to the long tail and should have its own acceptance threshold.\n\nJulian",
                ),
                message(
                    "James Park <james@northstar.example>",
                    "Julian <julian@js-lab.org>, Lisa Wang <lisa@northstar.example>",
                    "Wed, 11 Mar 2026 16:02:00 +0100",
                    "Staging has been on v2 reads for 24 hours. Search, export, and webhook delivery are clean; the only alert came from an intentionally expired test credential.\n\nJames",
                ),
                message(
                    "Lisa Wang <lisa@northstar.example>",
                    "James Park <james@northstar.example>, Julian <julian@js-lab.org>",
                    "Tue, 10 Mar 2026 08:55:00 +0100",
                    "I drafted the maintenance-window notice and the internal support brief. Both use April 7 as the target date, with April 14 held as the fallback.\n\nLisa",
                ),
                message(
                    OWN,
                    "Lisa Wang <lisa@northstar.example>, James Park <james@northstar.example>",
                    "Mon, 09 Mar 2026 17:31:00 +0100",
                    "April 7 works if the load test stays under the current error budget. Let’s make the go/no-go review explicit on the calendar rather than deciding in the deploy channel.\n\nJulian",
                ),
                message(
                    "James Park <james@northstar.example>",
                    "Julian <julian@js-lab.org>, Lisa Wang <lisa@northstar.example>",
                    "Sun, 08 Mar 2026 13:44:00 +0100",
                    "The first production-shaped load test is complete. Read throughput has 40% headroom, while the write path needs one more index before we can run the largest tenants safely.\n\nJames",
                ),
                message(
                    "Lisa Wang <lisa@northstar.example>",
                    "Julian <julian@js-lab.org>, James Park <james@northstar.example>",
                    "Sat, 07 Mar 2026 10:16:00 +0100",
                    "Here is the proposed migration timeline from today’s planning session. Engineering owns validation and rollback; Customer Success owns account sequencing and communication. Please mark any dependency we missed.\n\nLisa",
                ),
            ],
        ),
        conversation(
            "github-batch-export",
            "PR #247 merged: feat: add batch export",
            "GitHub, Julian",
            "Mar 22",
            &["inbox", "github"],
            false,
            vec![message(
                "GitHub <notifications@github.example>",
                OWN_EMAIL,
                "Sun, 22 Mar 2026 07:54:00 +0100",
                "Your pull request “feat: add batch export” was merged into main by @sarah-chen.\n\n14 files changed · 8 checks passed · merge commit 4f8c1d2",
            )],
        ),
        conversation(
            "march-invoice",
            "Invoice #2026-0891 — March 2026",
            "Cloudhost Billing",
            "Mar 21",
            &["inbox", "finance", "attachment"],
            false,
            vec![message(
                "Cloudhost Billing <billing@cloudhost.example>",
                OWN_EMAIL,
                "Sat, 21 Mar 2026 06:20:00 +0100",
                "Your March invoice is ready.\n\nAmount: EUR 1,247.00\nPayment due: April 15, 2026\n\nA PDF copy is attached for your records.",
            )],
        ),
        conversation(
            "sunday-lunch",
            "Lunch by the river?",
            "Nina Becker, Julian",
            "Mar 21",
            &["inbox", "personal"],
            false,
            vec![
                message(
                    "Nina Becker <nina@personal.example>",
                    OWN_EMAIL,
                    "Sat, 21 Mar 2026 12:14:00 +0100",
                    "Sunday looks sunny — shall we meet at the little café by the Isar around 12:30? I can bring the book I mentioned.\n\nNina",
                ),
                message(
                    OWN,
                    "Nina Becker <nina@personal.example>",
                    "Fri, 20 Mar 2026 19:02:00 +0100",
                    "I’m free this weekend. Lunch on Sunday would be perfect if the weather holds.\n\nJulian",
                ),
            ],
        ),
        conversation(
            "partner-workshop",
            "Re: Partner workshop agenda",
            "Priya Nair",
            "Mar 20",
            &["sent", "partnerships"],
            false,
            vec![message(
                OWN,
                "Priya Nair <priya@partner.example>",
                "Fri, 20 Mar 2026 14:36:00 +0100",
                "The agenda works for me. I’ve reserved 20 minutes for the integration walkthrough and another 15 for open questions from your solutions team.\n\nI’ll send the sample payload before Wednesday’s session.\n\nJulian",
            )],
        ),
        conversation(
            "advisory-board-outline",
            "Customer advisory board — discussion outline",
            "Morgan Lee",
            "Mar 19",
            &["draft", "customers"],
            false,
            vec![message(
                OWN,
                "Morgan Lee <morgan@customer.example>",
                "Thu, 19 Mar 2026 17:10:00 +0100",
                "Proposed outline for April:\n\n• What changed in the reporting workflow\n• Where approval handoffs still slow teams down\n• Two options for shared dashboards\n• Priorities for the next quarter\n\nCould you bring one recent reporting example for the group to discuss?",
            )],
        ),
        conversation(
            "berlin-itinerary",
            "Rail itinerary: Munich to Berlin",
            "Rail Desk",
            "Mar 12",
            &["archive", "travel", "attachment"],
            false,
            vec![message(
                "Rail Desk <bookings@rail.example>",
                OWN_EMAIL,
                "Thu, 12 Mar 2026 08:04:00 +0100",
                "Your itinerary for Munich to Berlin is confirmed.\n\nDeparture: April 6 at 07:56\nArrival: April 6 at 11:49\nCoach 12, seat 64\n\nThe ticket is attached.",
            )],
        ),
        conversation(
            "expired-conference-offer",
            "Last day for conference pricing",
            "Product Systems Conference",
            "Mar 3",
            &["deleted", "events"],
            false,
            vec![message(
                "Product Systems Conference <hello@events.example>",
                OWN_EMAIL,
                "Tue, 03 Mar 2026 09:00:00 +0100",
                "Early registration closes tonight. This year’s program covers design systems, observability, and platform engineering across two days in Hamburg.",
            )],
        ),
    ];
    let (_, thread) = mail
        .iter_mut()
        .find(|(p, _)| p.thread_id == "design-review")
        .unwrap();
    let message = &mut thread.messages[0];
    message.id = "demo-design".into();
    message.html = include_str!("../fixtures/review.html").into();
    message.attachments = vec![
        crate::data::Attachment {
            part_id: 1,
            filename: "dashboard-review.png".into(),
            content_type: "image/png".into(),
            size: include_bytes!("../fixtures/review.png").len() as u64,
            disposition: "inline".into(),
            content_id: "review-image".into(),
        },
        crate::data::Attachment {
            part_id: 2,
            filename: "review-notes.txt".into(),
            content_type: "text/plain".into(),
            size: include_bytes!("../fixtures/review.txt").len() as u64,
            ..Default::default()
        },
        crate::data::Attachment {
            part_id: 3,
            filename: "review-brief.pdf".into(),
            content_type: "application/pdf".into(),
            size: include_bytes!("../fixtures/review.pdf").len() as u64,
            ..Default::default()
        },
    ];
    mail[2].0.tags.push("attachment".into());
    mail
}

pub fn attachment(message_id: &str, part: u32) -> Result<Vec<u8>, String> {
    if message_id != "demo-design" {
        return Err("Sample attachment not found".into());
    }
    match part {
        1 => Ok(include_bytes!("../fixtures/review.png").to_vec()),
        2 => Ok(include_bytes!("../fixtures/review.txt").to_vec()),
        3 => Ok(include_bytes!("../fixtures/review.pdf").to_vec()),
        _ => Err("Sample attachment not found".into()),
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashSet;

    use super::*;
    use crate::data::FOLDERS;

    fn find<'a>(mail: &'a [(ThreadPreview, Thread)], id: &str) -> &'a (ThreadPreview, Thread) {
        mail.iter()
            .find(|(preview, _)| preview.thread_id == id)
            .unwrap()
    }

    #[test]
    fn design_review_is_newest_first_and_previewed_from_newest_message() {
        let mail = mail();
        let (preview, thread) = find(&mail, "design-review");

        assert_eq!(
            preview.preview,
            "The revised dashboard mockups are ready for review."
        );
        assert_eq!(preview.message_count, 3);
        assert_eq!(
            thread
                .messages
                .iter()
                .map(|message| message.from.as_str())
                .collect::<Vec<_>>(),
            [
                "Alex Rivera <alex@northstar.example>",
                OWN,
                "Alex Rivera <alex@northstar.example>",
            ]
        );
        assert_eq!(
            thread
                .messages
                .iter()
                .map(|message| message.date.as_str())
                .collect::<Vec<_>>(),
            [
                "Sun, 22 Mar 2026 10:33:00 +0100",
                "Sat, 21 Mar 2026 16:05:00 +0100",
                "Fri, 20 Mar 2026 11:12:00 +0100",
            ]
        );
    }

    #[test]
    fn migration_thread_has_distinct_content_and_a_long_newest_message() {
        let mail = mail();
        let (preview, thread) = find(&mail, "api-migration");
        let bodies = thread
            .messages
            .iter()
            .map(|message| message.body.as_str())
            .collect::<HashSet<_>>();
        let days = thread
            .messages
            .iter()
            .map(|message| {
                message
                    .date
                    .split_whitespace()
                    .nth(1)
                    .unwrap()
                    .parse::<u8>()
                    .unwrap()
            })
            .collect::<Vec<_>>();

        assert_eq!(thread.messages.len(), 16);
        assert_eq!(bodies.len(), thread.messages.len());
        assert_eq!(
            preview.preview,
            "Good call — I added a rollback section with step-by-step instructions."
        );
        assert!(thread.messages[0].body.len() > 1_200);
        assert_eq!(days, (7..=22).rev().collect::<Vec<_>>());
        assert_eq!(
            thread.messages.last().unwrap().date,
            "Sat, 07 Mar 2026 10:16:00 +0100"
        );
    }

    #[test]
    fn every_default_folder_has_meaningful_mail() {
        let mail = mail();

        for folder in FOLDERS {
            let tag = folder.query.strip_prefix("tag:").unwrap();
            let matches = mail
                .iter()
                .filter(|(preview, _)| preview.tags.iter().any(|item| item == tag));
            assert!(
                matches.into_iter().any(|(preview, thread)| {
                    !preview.subject.is_empty()
                        && !preview.preview.is_empty()
                        && !thread.messages.is_empty()
                        && thread
                            .messages
                            .iter()
                            .all(|message| !message.body.trim().is_empty())
                }),
                "missing useful demo mail for {}",
                folder.name
            );
        }
    }
}
