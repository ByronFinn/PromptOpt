# Documentation Language

The language engineering skills should write **human-facing output** in for this repo.

## Language

**中文（Chinese）**

All prose produced by skills — PRDs (`docs/prd/`), ADRs (`docs/adr/`), `CONTEXT.md`, issue titles and bodies, review reports, and comments — is written in Chinese.

## Scope

- **In scope**: human-facing prose in documents and issues.
- **Out of scope**: code, identifiers, file names, CLI commands, and configuration values. These follow code conventions, never this setting. `docs/agents/` config files and skill-internal operational templates remain in English (agent-facing).

## When a skill says "write the output"

Write the prose in Chinese. Do not silently switch to the conversation's language — the team reads the configured language, which may differ from whoever triggered the skill.

If a single artifact genuinely needs a second language (e.g. a bilingual README or release note for the open-source audience), the producing skill may deviate, but Chinese is always the primary one for internal docs.

## If this file is missing

Fall back to the user's input language, and ask which language the team prefers so it can be recorded here. Don't keep guessing every session.
