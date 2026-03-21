"""Database connection and session management."""

import os
from collections.abc import Generator
from contextlib import contextmanager
from pathlib import Path

from sqlalchemy import create_engine
from sqlalchemy.orm import Session, sessionmaker

from promptopt.storage.models import Base


class Database:
    """Database connection manager."""
    
    def __init__(self, db_path: str | None = None) -> None:
        """Initialize database.
        
        Args:
            db_path: Path to SQLite database file. If None, uses default location.
        """
        if db_path is None:
            home_dir = Path.home()
            promptopt_dir = home_dir / ".promptopt"
            promptopt_dir.mkdir(parents=True, exist_ok=True)
            db_path = str(promptopt_dir / "promptopt.db")

        db_file = Path(db_path).expanduser()
        if not db_file.is_absolute():
            db_file = db_file.resolve()
        db_file.parent.mkdir(parents=True, exist_ok=True)
        
        self.db_path = str(db_file)
        self.engine = create_engine(
            f"sqlite:///{self.db_path}",
            echo=os.getenv("DEBUG", "0") == "1",
        )
        self.session_factory = sessionmaker(bind=self.engine, expire_on_commit=False)
    
    def create_tables(self) -> None:
        """Create all tables."""
        Base.metadata.create_all(self.engine)
    
    @contextmanager
    def session(self) -> Generator[Session]:
        """Get a database session.
        
        Yields:
            SQLAlchemy session
        """
        session = self.session_factory()
        try:
            yield session
            session.commit()
        except Exception:
            session.rollback()
            raise
        finally:
            session.close()
    
    def close(self) -> None:
        """Close database connections."""
        self.engine.dispose()


# Global database instance
_db: Database | None = None


def get_db(db_path: str | None = None) -> Database:
    """Get or create global database instance."""
    global _db
    normalized_path = None if db_path is None else str(Path(db_path).expanduser())

    if _db is None:
        _db = Database(normalized_path)
        _db.create_tables()
    elif normalized_path is not None and Path(_db.db_path) != Path(normalized_path):
        _db.close()
        _db = Database(normalized_path)
        _db.create_tables()
    return _db


def reset_db() -> None:
    """Reset global database instance."""
    global _db
    if _db:
        _db.close()
        _db = None
