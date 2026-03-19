# Python 3.14 Code Rules

- Target Python 3.14 only.
- Prefer simple, readable, maintainable code.
- Use modern typing only: `list[int]`, `dict[str, int]`, `X | None`, `type Alias = ...`.
- Prefer `collections.abc` abstract types for inputs (`Sequence`, `Mapping`, `Iterable`, `Callable`).
- Type all public APIs precisely; avoid `Any` unless unavoidable.
- Use `Protocol` for structural interfaces, ABC for explicit inheritance.
- Use `NewType` only for semantic IDs/tokens, not runtime validation.
- Prefer `@dataclass(slots=True)` for data containers; use `field(default_factory=...)` for mutable defaults.
- Use normal classes when behavior or invariants matter.
- Mark class constants with `ClassVar`.
- Never use mutable default arguments.
- Raise specific exceptions and preserve chains with `raise ... from exc`.
- Prefer exceptions over custom Result patterns unless explicitly requested.
- Use `async` only for real I/O; manage resources with `async with` and timeouts.
- Decorators must preserve signature with `ParamSpec` and `functools.wraps`.
- Naming: `snake_case` for funcs/vars, `CapWords` for classes/types, `UPPER_SNAKE_CASE` for constants.
- Prefer `_internal`; avoid `__mangled` unless necessary.
- Avoid legacy typing (`List`, `Dict`, `Optional`, `TypeAlias`) and unnecessary complexity.