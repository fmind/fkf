# Shared normalization for agent-prompts.sh metadata and agent-prompt-body.sh lazy reads. Keeping one
# filter is a security boundary: a body lookup must never reintroduce harness blocks that the
# collector classified as injected rather than as the user's request.

def strip_harness_block($tag):
  if contains("<" + $tag)
  then gsub("<" + $tag + "(\\s[^>]*)?>.*?</" + $tag + ">"; ""; "m")
  else . end;

def dechrome_agent_prompt:
  strip_harness_block("system-reminder")
  | strip_harness_block("ADDITIONAL_METADATA")
  | strip_harness_block("USER_SETTINGS_CHANGE")
  | strip_harness_block("task-notification")
  | strip_harness_block("local-command-caveat")
  | strip_harness_block("local-command-stdout")
  | strip_harness_block("command-name")
  | strip_harness_block("command-message")
  | strip_harness_block("command-args")
  | strip_harness_block("recommended_plugins")
  | strip_harness_block("environment_context")
  | strip_harness_block("user-prompt-submit-hook")
  | strip_harness_block("ide_opened_file")
  | strip_harness_block("ide_selection")
  | strip_harness_block("codex_internal_context")
  | if contains("USER_REQUEST") then gsub("</?USER_REQUEST>"; "") else . end;

def is_injected_agent_prompt:
  . as $text
  | ["# AGENTS.md instructions for",
     "Are you still working on",
     "Your claude.ai usage limit",
     "Caveat: The messages below are auto-generated",
     "[Request interrupted",
     "This session is being continued from a previous",
     "API Error:",
     "<system>"]
  | any(. as $prefix | $text | startswith($prefix));

def normalize_agent_prompt:
  dechrome_agent_prompt
  | sub("^[ \t\n\r]+"; "")
  | select(. != "")
  | select(is_injected_agent_prompt | not);
