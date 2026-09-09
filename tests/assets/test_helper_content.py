"""Title projection and lazy-body behavior of bundled helpers."""

from __future__ import annotations

import base64
import json
import os
from pathlib import Path

import pytest

from fkf.assets import read_asset

from .conftest import HelperInstallation, load_preset, validate_helper_output


@pytest.mark.parametrize(
    ("helper", "provider_output", "field", "expected", "source"),
    [
        (
            "github-gists-json.sh",
            (
                '[{"id":"deadbeef","description":"","html_url":"https://gist.github.com/example/deadbeef",'
                '"updated_at":"2026-05-04T09:00:00Z","public":true,"files":{"notes.md":{}}}]'
            ),
            "description",
            "Gist deadbeef",
            "github-gists",
        ),
        (
            "github-stars-json.sh",
            (
                '[{"starred_at":"2026-05-04T09:00:00Z","repo":{"full_name":"example/project",'
                '"html_url":"https://github.com/example/project","description":"Soft\\u00adware",'
                '"language":"Go","topics":[],"stargazers_count":7,"archived":false}}]'
            ),
            "title",
            "example/project: Software",
            "github-stars",
        ),
    ],
)
def test_github_titles_are_stable_and_visible(
    tmp_path: Path,
    helpers: HelperInstallation,
    helper: str,
    provider_output: str,
    field: str,
    expected: str,
    source: str,
) -> None:
    helpers.fake("github-generic-list-json.py", 'printf "%s\\n" "$FAKE_RESPONSE"\n')
    result = helpers.run(helper, environment={"FAKE_RESPONSE": provider_output})
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    record = json.loads(result.stdout)
    assert record[field] == expected
    document = validate_helper_output(load_preset(tmp_path, "personal"), source, result.stdout)
    assert document.count == 1


def test_calendar_and_gmail_titles_are_total_and_visible(tmp_path: Path, helpers: HelperInstallation) -> None:
    helpers.fake(
        "gws",
        """case "$*" in
  *"calendarList list"*)
    printf '%s\n' '{"items":[{"id":"calendar@example.test","summary":"Calendar","accessRole":"owner"}]}' ;;
  *"events list"*)
    printf '%s\n' '{"items":[{"id":"empty-summary","summary":"   ","status":"confirmed","eventType":"default","start":{"dateTime":"2026-05-04T09:00:00Z"},"end":{"dateTime":"2026-05-04T10:00:00Z"}}]}' ;;
  *) exit 2 ;;
esac
""",
    )
    calendar = helpers.run(
        "gws-calendars-json.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        "2026-05-04",
        "2026-05-05",
    )
    assert calendar.returncode == 0, calendar.stderr.decode(errors="replace")
    calendar_records = json.loads(calendar.stdout)
    assert calendar_records[0]["summary"] == "Calendar event empty-summary"
    personal = load_preset(tmp_path, "personal")
    assert validate_helper_output(personal, "google-calendar-agenda", calendar.stdout).count == 1
    assert validate_helper_output(personal, "google-calendar-events", calendar.stdout).count == 1

    helpers.fake(
        "gws",
        """case "$*" in
  *"users messages list"*) printf '%s\n' '{"messages":[{"id":"message-1"}]}' ;;
  *'"id":"message-1"'*)
    printf '%s\n' '{"id":"message-1","threadId":"thread-1","internalDate":"1777885200000","payload":{"headers":[{"name":"Subject","value":"Zero\\u200dWidth"}]}}' ;;
  *) exit 2 ;;
esac
""",
    )
    gmail = helpers.run("gmail-json.py", "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
    assert gmail.returncode == 0, gmail.stderr.decode(errors="replace")
    assert json.loads(gmail.stdout)["subject"] == "ZeroWidth"
    assert validate_helper_output(personal, "google-gmail-emails", gmail.stdout).count == 1


def test_gmail_preserves_provider_formatted_recipients(tmp_path: Path, helpers: HelperInstallation) -> None:
    helpers.fake(
        "gws",
        """case "$*" in
  *"users messages list"*) printf '%s\n' '{"messages":[{"id":"message-1"}]}' ;;
  *'"id":"message-1"'*) printf '%s\n' "$GMAIL_MESSAGE" ;;
  *) exit 2 ;;
esac
""",
    )
    message = {
        "id": "message-1",
        "threadId": "thread-1",
        "internalDate": "1777885200000",
        "payload": {
            "headers": [
                {"name": "Subject", "value": "Mailbox preservation"},
                {"name": "From", "value": "Jane Doe <jane@example.test>"},
                {
                    "name": "To",
                    "value": (
                        'Jane Doe <JANE@example.test>, "Doe, John" <john@example.test>, John <"john..doe"@example.test>'
                    ),
                },
                {
                    "name": "Cc",
                    "value": (
                        'Other <other@example.test>, Leading <".john"@Example.test>, '
                        'Trailing <"john."@example.test>, PLAIN@Example.test'
                    ),
                },
            ]
        },
    }
    result = helpers.run(
        "gmail-json.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        environment={"GMAIL_MESSAGE": json.dumps(message, separators=(",", ":"))},
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    record = json.loads(result.stdout)
    assert record["from"] == "Jane Doe <jane@example.test>"
    assert record["to"] == [
        "Jane Doe <JANE@example.test>",
        '"Doe, John" <john@example.test>',
        'John <"john..doe"@example.test>',
        "Other <other@example.test>",
        'Leading <".john"@Example.test>',
        'Trailing <"john."@example.test>',
        "PLAIN@Example.test",
    ]
    assert record["participant_uris"] == [
        "person:email/%22.john%22@example.test",
        "person:email/%22john.%22@example.test",
        "person:email/%22john..doe%22@example.test",
        "person:email/jane@example.test",
        "person:email/john@example.test",
        "person:email/other@example.test",
        "person:email/plain@example.test",
    ]
    assert validate_helper_output(load_preset(tmp_path, "personal"), "google-gmail-emails", result.stdout).count == 1


def test_gmail_preserves_smtputf8_recipients(tmp_path: Path, helpers: HelperInstallation) -> None:
    helpers.fake(
        "gws",
        """case "$*" in
  *"users messages list"*) printf '%s\n' '{"messages":[{"id":"message-1"}]}' ;;
  *'"id":"message-1"'*) printf '%s\n' "$GMAIL_MESSAGE" ;;
  *) exit 2 ;;
esac
""",
    )
    message = {
        "id": "message-1",
        "threadId": "thread-1",
        "internalDate": "1777885200000",
        "payload": {
            "headers": [
                {"name": "Subject", "value": "SMTPUTF8 preservation"},
                {"name": "From", "value": "Sender <SENDER@example.test>"},
                {"name": "To", "value": "Jöhn <jöhn@Example.test>"},
                {"name": "Cc", "value": '"Dœ, Jane" <JANE@example.test>'},
            ]
        },
    }
    result = helpers.run(
        "gmail-json.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        environment={"GMAIL_MESSAGE": json.dumps(message, separators=(",", ":"))},
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    record = json.loads(result.stdout)
    assert record["to"] == [
        "Jöhn <jöhn@Example.test>",
        '"Dœ, Jane" <JANE@example.test>',
    ]
    assert record["participant_uris"] == [
        "person:email/j%C3%B6hn@example.test",
        "person:email/jane@example.test",
        "person:email/sender@example.test",
    ]
    assert validate_helper_output(load_preset(tmp_path, "personal"), "google-gmail-emails", result.stdout).count == 1


def test_gmail_preserves_groups_domain_literals_and_quoted_at(tmp_path: Path, helpers: HelperInstallation) -> None:
    helpers.fake(
        "gws",
        """case "$*" in
  *"users messages list"*) printf '%s\n' '{"messages":[{"id":"message-1"}]}' ;;
  *'"id":"message-1"'*) printf '%s\n' "$GMAIL_MESSAGE" ;;
  *) exit 2 ;;
esac
""",
    )
    message = {
        "id": "message-1",
        "threadId": "thread-1",
        "internalDate": "1777885200000",
        "payload": {
            "headers": [
                {"name": "Subject", "value": "Address grammar"},
                {
                    "name": "To",
                    "value": ('Friends: Literal <user@[127.0.0.1]>, Quoted <"local@part"@Example.test>;'),
                },
                {
                    "name": "Cc",
                    "value": (
                        "Outside <OUT@example.test>, a(comment)@(comment)example.com, "
                        "=?utf-8?q?J=C3=B6hn?= <encoded@example.test>, "
                        'Routed <@route1,@route2:routed@example.test>, QuotedDots <"route..neighbor"@Example.test>'
                    ),
                },
            ]
        },
    }
    result = helpers.run(
        "gmail-json.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
        environment={"GMAIL_MESSAGE": json.dumps(message, separators=(",", ":"))},
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    record = json.loads(result.stdout)
    assert record["to"] == [
        "Literal <user@[127.0.0.1]>",
        'Quoted <"local@part"@Example.test>',
        "Outside <OUT@example.test>",
        "a@example.com",
        "Jöhn <encoded@example.test>",
        "Routed <routed@example.test>",
        'QuotedDots <"route..neighbor"@Example.test>',
    ]
    assert record["participant_uris"] == [
        "person:email/%22local@part%22@example.test",
        "person:email/%22route..neighbor%22@example.test",
        "person:email/a@example.com",
        "person:email/encoded@example.test",
        "person:email/out@example.test",
        "person:email/routed@example.test",
        "person:email/user@%5B127.0.0.1%5D",
    ]
    assert validate_helper_output(load_preset(tmp_path, "personal"), "google-gmail-emails", result.stdout).count == 1

    for invalid in ("Broken <a@@example.test>", "evil@example.test\nBcc: victim@example.test"):
        message["payload"]["headers"][1]["value"] = invalid
        malformed = helpers.run(
            "gmail-json.py",
            "2026-05-04T00:00:00Z",
            "2026-05-05T00:00:00Z",
            environment={"GMAIL_MESSAGE": json.dumps(message, separators=(",", ":"))},
        )
        assert malformed.returncode != 0
        assert malformed.stdout == b""
        assert b"invalid mailbox header" in malformed.stderr


def test_rss_titles_drop_invisible_format_characters(tmp_path: Path, helpers: HelperInstallation) -> None:
    feed = tmp_path / "feed.xml"
    feed.write_text(
        '<rss version="2.0"><channel><title>Exa\u200bmple</title><link>https://example.test</link>'
        "<item><guid>post-1</guid><title>Po\u200bst</title><link>https://example.test/post</link>"
        "<pubDate>Mon, 04 May 2026 09:00:00 +0000</pubDate></item></channel></rss>",
        encoding="utf-8",
    )
    helpers.fake(
        "curl",
        """output=
while [ "$#" -gt 0 ]; do
  case "$1" in --output) output=$2; shift 2 ;; *) shift ;; esac
done
cp "$RSS_FIXTURE" "$output"
""",
    )
    result = helpers.run(
        "rss-json.py",
        "https://example.test/feed.xml",
        environment={"RSS_FIXTURE": os.fspath(feed)},
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = json.loads(result.stdout)
    assert [record["title"] for record in records] == ["Example", "Post"]
    assert validate_helper_output(load_preset(tmp_path, "personal"), "rss-items", result.stdout).count == 2


def test_github_commit_normalizes_noreply_actor_and_title(tmp_path: Path, helpers: HelperInstallation) -> None:
    helpers.fake(
        "gh",
        """printf '%s\n' '[{"sha":"abcdef0123456789","url":"https://github.com/fmind/fkf/commit/abc","repository":{"fullName":"fmind/fkf"},"commit":{"author":{"date":"2026-05-04T09:00:00Z","email":"12345+Fmind@users.noreply.github.com"},"message":"\\n\\u200d\\tfix: preserve\\tcomplete windows\\r\\nBody"}}]'
""",
    )
    result = helpers.run(
        "github-commits-json.py",
        "2026-05-04T00:00:00Z",
        "2026-05-05T00:00:00Z",
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = json.loads(result.stdout)
    assert records[0]["participant_uris"] == ["actor:github.com/fmind"]
    assert records[0]["title"] == "fix: preserve complete windows"
    assert validate_helper_output(load_preset(tmp_path, "personal"), "github-commits", result.stdout).count == 1


def test_calendar_body_splits_last_separator_and_redacts_provider_failure(helpers: HelperInstallation) -> None:
    call_log = helpers.root / "gws-call"
    helpers.fake(
        "gws",
        """printf '%s\n' "$*" > "$CALL_LOG"
printf '%s\n' '{"id":"event-1","description":"Agenda","location":"Room 7","hangoutLink":"https://meet.example.test/one"}'
""",
    )
    result = helpers.run(
        "gws-calendar-body.py",
        "team~archive@example.test~event-1",
        environment={"CALL_LOG": os.fspath(call_log)},
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert result.stdout == b"Agenda\n\nLocation: Room 7\n\nConference: https://meet.example.test/one\n"
    call = call_log.read_text(encoding="utf-8")
    assert '"calendarId":"team~archive@example.test"' in call
    assert '"eventId":"event-1"' in call

    helpers.fake("gws", "printf '%s\\n' 'private provider diagnostic' >&2\nexit 7\n")
    failed = helpers.run("gws-calendar-body.py", "owner@example.test~event-1")
    assert failed.returncode != 0
    assert failed.stdout == b""
    assert b"cannot fetch the calendar event" in failed.stderr
    assert b"private provider diagnostic" not in failed.stderr


def test_gmail_body_prefers_raw_plain_text_and_exact_provider_request(helpers: HelperInstallation) -> None:
    message = (
        b"From: sender@example.test\r\nTo: owner@example.test\r\nSubject: Example\r\n"
        b"Content-Type: text/plain; charset=utf-8\r\n\r\nHello from Gmail.\r\n"
    )
    encoded = base64.urlsafe_b64encode(message).rstrip(b"=").decode()
    call_log = helpers.root / "gws-call"
    helpers.fake(
        "gws",
        'printf "%s\\n" "$*" > "$CALL_LOG"\nprintf "%s\\n" "$GMAIL_RESPONSE"\n',
    )
    result = helpers.run(
        "gmail-body.py",
        "message-1",
        environment={
            "CALL_LOG": os.fspath(call_log),
            "GMAIL_RESPONSE": json.dumps({"id": "message-1", "raw": encoded}, separators=(",", ":")),
        },
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert result.stdout == b"Hello from Gmail.\n"
    call = call_log.read_text(encoding="utf-8")
    assert '"id":"message-1"' in call
    assert '"format":"raw"' in call


def test_message_and_document_bodies_validate_identity_and_keep_visible_text(helpers: HelperInstallation) -> None:
    helpers.fake(
        "gws",
        """case "$*" in
  *"chat spaces messages get"*)
    printf '%s\n' '{"name":"spaces/one/messages/message-1","formattedText":"Visible chat body"}' ;;
  *"docs documents get"*)
    printf '%s\n' '{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"First paragraph.\\n"}}]}},{"table":{"tableRows":[{"tableCells":[{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Table cell.\\n"}}]}}]}]}]}}]}}' ;;
  *) exit 2 ;;
esac
""",
    )
    chat = helpers.run("gws-chat-message-body.py", "spaces/one/messages/message-1")
    assert chat.returncode == 0, chat.stderr.decode(errors="replace")
    assert chat.stdout == b"Visible chat body\n"
    invalid = helpers.run("gws-chat-message-body.py", "../message-1")
    assert invalid.returncode == 2
    assert invalid.stdout == b""
    assert b"invalid message resource name" in invalid.stderr

    document = helpers.run("gws-doc-text.sh", "document-1")
    assert document.returncode == 0, document.stderr.decode(errors="replace")
    assert document.stdout == b"First paragraph.\nTable cell.\n\n"


def test_agent_memory_body_is_confined_and_bounded(helpers: HelperInstallation) -> None:
    memory = helpers.home / ".codex" / "memories" / "rollout_summaries"
    memory.mkdir(parents=True)
    allowed = memory / "session.md"
    allowed.write_text("reviewed memory\n", encoding="utf-8")
    accepted = helpers.run("agent-memory-body.py", os.fspath(allowed))
    assert accepted.returncode == 0
    assert accepted.stdout == b"reviewed memory\n"

    grok_memory = helpers.home / ".grok" / "memory" / "project"
    grok_memory.mkdir(parents=True)
    grok = grok_memory / "session.md"
    grok.write_text("reviewed Grok memory\n", encoding="utf-8")
    assert helpers.run("agent-memory-body.py", os.fspath(grok)).stdout == b"reviewed Grok memory\n"

    outside = helpers.home / "outside.md"
    outside.write_text("outside\n", encoding="utf-8")
    linked = memory / "linked.md"
    linked.symlink_to(outside)
    for refused in (outside, linked):
        result = helpers.run("agent-memory-body.py", os.fspath(refused))
        assert result.returncode != 0
        assert result.stdout == b""
        assert b"outside the reviewed" in result.stderr or b"absent, linked" in result.stderr

    oversized = memory / "oversized.md"
    with oversized.open("wb") as stream:
        stream.truncate((4 << 20) + 1)
    result = helpers.run("agent-memory-body.py", os.fspath(oversized))
    assert result.returncode != 0
    assert result.stdout == b""
    assert b"exceeds the 4194304-byte body limit" in result.stderr


def test_provider_body_reads_enforce_the_command_bound_while_children_run() -> None:
    calendar = read_asset("presets/sources/gws-calendar-body.py")
    assert b".read(MAX_PROVIDER_BYTES + 1)" in calendar
    assert b"process.kill()" in calendar
    gmail = read_asset("presets/sources/gmail-body.py")
    assert b".read(MAX_PROVIDER_BYTES + 1)" in gmail
    assert b"process.kill()" in gmail
