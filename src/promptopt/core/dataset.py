"""Dataset loading functionality."""

from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

import yaml

from promptopt.core.task import Split


@dataclass(slots=True)
class Sample:
    """A single sample from a dataset.
    
    Attributes:
        id: Unique sample identifier
        input: Input text for the prompt
        expected: Expected output/answer
        split: Dataset split (dev/test/train)
    """
    id: str
    input: str
    expected: str | dict[str, object]
    split: Split


@dataclass(slots=True)
class DatasetLoader:
    """Loads datasets from JSON/YAML/CSV files.
    
    Supports filtering by split type and returns a list of Sample objects.
    
    Attributes:
        path: Path to the dataset file
        split_field: Field name containing split information
    """
    path: str
    split_field: str = "split"
    
    def load(self, split: Split | None = None) -> Sequence[Sample]:
        """Load samples from the dataset file.
        
        Args:
            split: If provided, only return samples from this split.
                   If None, return all samples.
        
        Returns:
            List of Sample objects matching the filter criteria
        
        Raises:
            FileNotFoundError: If the dataset file doesn't exist
            ValueError: If the file format is unsupported or data is invalid
        """
        file_path = Path(self.path)
        
        if not file_path.exists():
            raise FileNotFoundError(f"Dataset file not found: {self.path}")
        
        suffix = file_path.suffix.lower()
        
        if suffix in (".yaml", ".yml"):
            samples = self._load_yaml(file_path)
        elif suffix == ".json":
            samples = self._load_json(file_path)
        elif suffix == ".csv":
            samples = self._load_csv(file_path)
        else:
            raise ValueError(f"Unsupported file format: {suffix}")
        
        if split is not None:
            samples = [s for s in samples if s.split == split]
        
        return samples
    
    def _load_yaml(self, path: Path) -> list[Sample]:
        """Load samples from a YAML file."""
        with open(path, encoding="utf-8") as f:
            data = yaml.safe_load(f)
        
        return self._parse_data(data)
    
    def _load_json(self, path: Path) -> list[Sample]:
        """Load samples from a JSON file."""
        import json
        
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
        
        return self._parse_data(data)
    
    def _load_csv(self, path: Path) -> list[Sample]:
        """Load samples from a CSV file."""
        import csv
        
        samples: list[Sample] = []
        
        with open(path, newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f)
            
            for row in reader:
                split_value = row.get(self.split_field, Split.DEV.value)
                
                try:
                    split = Split(split_value.lower())
                except ValueError:
                    split = Split.DEV
                
                samples.append(
                    Sample(
                        id=row.get("id", f"sample_{len(samples)}"),
                        input=row.get("input", ""),
                        expected=row.get("expected", ""),
                        split=split,
                    )
                )
        
        return samples
    
    def _parse_data(self, data: dict[str, object]) -> list[Sample]:
        """Parse samples from loaded data structure.
        
        Expected format:
            {"samples": [{"id": "...", "input": "...", "expected": "...", "split": "..."}]}
        Or:
            [{"id": "...", "input": "...", "expected": "...", "split": "..."}]
        """
        if isinstance(data, list):
            sample_dicts = data
        elif isinstance(data, dict):
            sample_dicts = data.get("samples", [])
            if not isinstance(sample_dicts, list):
                raise ValueError("Data must contain a 'samples' list or be a list itself")
        else:
            raise ValueError(f"Unexpected data type: {type(data)}")
        
        samples: list[Sample] = []
        
        for sample_dict in sample_dicts:
            if not isinstance(sample_dict, dict):
                raise ValueError(f"Sample must be a dict, got: {type(sample_dict)}")
            
            id_str = sample_dict.get("id")
            if not id_str:
                id_str = f"sample_{len(samples)}"
            
            input_str = sample_dict.get("input", "")
            expected_val = sample_dict.get("expected", "")
            split_str = sample_dict.get(self.split_field, Split.DEV.value)
            
            try:
                split = Split(split_str.lower())
            except (ValueError, AttributeError):
                split = Split.DEV
            
            samples.append(
                Sample(
                    id=str(id_str),
                    input=str(input_str),
                    expected=expected_val,
                    split=split,
                )
            )
        
        return samples
