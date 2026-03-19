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
            promptopt_dir.mkdir(exist_ok=True)
            db_path = str(promptopt_dir / "promptopt.db")
        
        self.db_path = db_path
        self.engine = create_engine(
            f"sqlite:///{db_path}",
            echo=os.getenv("DEBUG", "0") == "1",
        )
        self.session_factory = sessionmaker(bind=self.engine)
    
    def create_tables(self) -> None:
        """Create all tables."""
        Base.metadata.create_all(self.engine)
    
    @contextmanager
    def session(self) -> Generator[Session, None, None]:
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


def get_db() -> Database:
    """Get or create global database instance."""
    global _db
    if _db is None:
        _db = Database()
        _db.create_tables()
    return _db


def reset_db() -> None:
    """Reset global database instance."""
    global _db
    if _db:
        _db.close()
        _db = None
