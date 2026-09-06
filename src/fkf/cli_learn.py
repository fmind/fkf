"""CLI adapters for reviewable, trace-backed learning proposals."""

from __future__ import annotations

from typing import Annotated

import typer

from fkf.cli_support import FKFGroup, parent_without_command, state
from fkf.errors import InvalidUsageError
from fkf.learn import (
    LearnActionReport,
    LearnAppliedCanceledError,
    LearnProposalReport,
    LearnRebuildError,
    LearnReview,
    apply_learn,
    propose_learn,
    reject_learn,
    review_learn,
)
from fkf.locking import WriterLock
from fkf.output import register_jsonl, register_text


def _proposal_text(report: LearnProposalReport) -> str:
    if report.nothing_to_propose:
        return "nothing to propose"
    if report.dry_run:
        lines = [f"- {item.text} · {item.trace}#learned -> {item.target}" for item in report.candidates]
        lines.extend(("", f"{len(report.candidates)} candidate log bullet(s); nothing written"))
        return "\n".join(lines)
    if report.proposal is None:
        raise RuntimeError("learn proposal report has no staged proposal")
    status = "already staged" if report.existing else "staged"
    proposal = report.proposal
    return "\n".join(
        (
            f"{status} {proposal.id} · {proposal.bytes} bytes · {proposal.path}",
            f"review: fkf learn review {proposal.id} --diff",
        )
    )


def _review_text(review: LearnReview) -> str:
    if not review.proposals:
        return "no active learn proposals"
    if any(proposal.diff for proposal in review.proposals):
        return "".join(
            proposal.diff or f"{proposal.id} · {proposal.bytes} bytes · {proposal.path} · {', '.join(proposal.files)}\n"
            for proposal in review.proposals
        )
    return "\n".join(
        f"{proposal.id} · {proposal.bytes} bytes · {proposal.path} · {', '.join(proposal.files)}"
        for proposal in review.proposals
    )


def _action_text(report: LearnActionReport) -> str:
    lines = [f"{report.status} {report.id} · {report.path}"]
    if report.rebuild_error:
        lines.extend((f"cache rebuild failed: {report.rebuild_error}", "repair: fkf build"))
    if report.files:
        lines.append(f"files: {', '.join(report.files)}")
    return "\n".join(lines)


# Learn envelopes are intentionally one JSONL record, matching Go's absence of a stream selector.
register_jsonl(LearnProposalReport, lambda report: (report,))
register_jsonl(LearnReview, lambda review: (review,))
register_jsonl(LearnActionReport, lambda report: (report,))
register_text(LearnProposalReport, _proposal_text)
register_text(LearnReview, _review_text)
register_text(LearnActionReport, _action_text)


def register_learn_commands(app: typer.Typer) -> None:
    """Attach the proposal workflow while keeping all read-only paths lock-free."""

    learn_app = typer.Typer(
        cls=FKFGroup,
        invoke_without_command=True,
        no_args_is_help=False,
        help=(
            "Stage, review, apply, or reject reviewable knowledge diffs. Keeps agent-authored changes out of "
            "durable knowledge until a person reviews an exact unified diff. Active proposals live under "
            ".agents/tmp/learn and may target only flat wiki or projects pages."
        ),
        rich_markup_mode=None,
    )
    app.add_typer(learn_app, name="learn")

    @learn_app.callback()
    def learn_parent(ctx: typer.Context) -> None:
        parent_without_command(ctx, "name a subcommand")

    @learn_app.command(
        "propose",
        help=(
            "Build one deterministic proposal for wiki/log.md and cite every task trace in sources frontmatter. "
            "--dry-run lists the candidate bullets and creates no queue directory."
        ),
        short_help="Stage unharvested task lessons as a wiki log diff.  [writes the base]",
    )
    def propose_command(
        ctx: typer.Context,
        dry_run: Annotated[
            bool,
            typer.Option("--dry-run", help="List candidate log bullets and write nothing."),
        ] = False,
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        if dry_run:
            invocation.emit(propose_learn(base, dry_run=True, cancel=invocation.cancel))
            return
        with WriterLock.acquire(base.root):
            invocation.emit(propose_learn(base, cancel=invocation.cancel))

    @learn_app.command(
        "review",
        help=(
            "With no proposal, list the active queue. With one proposal and --diff, print the bounded unified "
            "diff that apply would validate."
        ),
        short_help="List queued proposals or inspect one exact diff.",
    )
    def review_command(
        ctx: typer.Context,
        proposal: Annotated[str | None, typer.Argument(help="Exact proposal id to inspect.")] = None,
        include_diff: Annotated[
            bool,
            typer.Option("--diff", help="Include the exact unified diff; requires one proposal id."),
        ] = False,
    ) -> None:
        if include_diff and proposal is None:
            raise InvalidUsageError("usage: fkf learn review <proposal> --diff")
        invocation = state(ctx)
        invocation.emit(
            review_learn(
                invocation.base(),
                proposal or "",
                include_diff=include_diff,
                cancel=invocation.cancel,
            )
        )

    @learn_app.command(
        "apply",
        help=(
            "Accept only unified diffs against flat wiki/*.md and projects/*.md pages. Roll back on patch "
            "mismatch, validation failure, or archive failure. A cache failure leaves the approved edit "
            "applied; run fkf build or repeat apply to repair it."
        ),
        short_help="Validate, apply, and archive one reviewed proposal.  [writes the base]",
    )
    def apply_command(
        ctx: typer.Context,
        proposal: Annotated[str, typer.Argument(help="Exact reviewed proposal id.")],
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        with WriterLock.acquire(base.root):
            try:
                report = apply_learn(base, proposal, cancel=invocation.cancel)
            except LearnRebuildError as error:
                invocation.emit(error.report)
                raise
            except LearnAppliedCanceledError as error:
                invocation.emit(error.report)
                raise
            invocation.emit(report)

    @learn_app.command(
        "reject",
        help="Archive one proposal without changing knowledge.",
        short_help="Archive one proposal without changing knowledge.  [writes the base]",
    )
    def reject_command(
        ctx: typer.Context,
        proposal: Annotated[str, typer.Argument(help="Exact reviewed proposal id.")],
    ) -> None:
        invocation = state(ctx)
        base = invocation.base()
        with WriterLock.acquire(base.root):
            invocation.emit(reject_learn(base, proposal, cancel=invocation.cancel))


__all__ = ["register_learn_commands"]
