# AGENTS.md

This file helps Autohand understand how to work with this project.

## Project Overview

- **Language**: Go
- **Package Manager**: go

## Commands

- **Build**: `go build`
- **Run**: `go run .`
- **Test**: `go test ./...`
- **Format**: `go fmt ./...`
- **Vet**: `go vet ./...`

## Instruction Sources

- Check saved memories and preferences before implementation work.
- Follow this AGENTS.md file for repository-specific guidance.
- AGENTS.md takes precedence over CLAUDE.md when both files provide instructions.

## Code Style

- Follow Go idioms and conventions
- Use short variable names in small scopes
- Handle errors explicitly
- Follow existing patterns in the codebase
- Use meaningful variable and function names
- Add comments for complex logic
- Keep functions focused and small

## Constraints

- Do not modify files outside the project directory
- Ask before making breaking changes
- Prefer editing existing files over creating new ones
- Do not delete files without confirmation
- Keep dependencies minimal - avoid adding new ones without good reason
- Do not commit sensitive data (API keys, secrets, credentials)
