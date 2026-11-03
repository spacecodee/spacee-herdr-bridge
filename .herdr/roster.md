# herdr agents

Mode: multi

| name | kind | role | dir |
| --- | --- | --- | --- |
| Orchestator - B | claude-code | plans, writes specs, delegates, verifies, deploys, commits | . |
| Implementer - B | claude-code | bridge (Go) tasks from docs/superpowers/specs | . |
| Implementer - F | claude-code | frontend (Angular) tasks from docs/superpowers/specs | ../spacee-herdr-frontend |
