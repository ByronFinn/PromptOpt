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
                split_str = row.get(self.split_field) or "dev"
                
                try:
                    split = Split(split_str.lower())
                except ValueError:
                    split = Split("dev")
                
                samples.append(
                    Sample(
                        id=row.get("id", f"sample_{len(samples)}") or f"sample_{len(samples)}",
                        input=row.get("input", "") or "",
                        expected=row.get("expected", "") or "",
                        split=split,
                    )
                )
        
        return samples
    
    def _load_dataset_config(self, config: dict[str, object], split_field: str) -> list[Sample]:
        """Load dataset from a Dataset config format.
        
        Args:
            config: Dataset config dict with name, path, split_field
            split_field: Field name for split (may come from config or self)
        
        Returns:
            List of Sample objects from the referenced data file
        """
        data_path = config.get("path")
        if not data_path or not isinstance(data_path, str):
            raise ValueError("Dataset config must contain a valid 'path' field")
        
        # Try multiple path resolution strategies
        config_dir = Path(self.path).parent
        project_root = config_dir.parent  # Parent of the config directory
        
        # Strategy 1: Relative to config file's directory
        resolved_path = (config_dir / data_path).resolve()
        if resolved_path.exists():
            pass  # Use this path
        # Strategy 2: Relative to project root (config_dir's parent)
        elif (project_root / data_path).exists():
            resolved_path = (project_root / data_path).resolve()
        # Strategy 3: Relative to current working directory
        elif Path(data_path).exists():
            resolved_path = Path(data_path).resolve()
        else:
            raise FileNotFoundError(f"Dataset file not found: {data_path} (tried {resolved_path}, {project_root / data_path})")
        
        suffix = resolved_path.suffix.lower()
        
        # Use split_field from config if available, otherwise use self.split_field
        effective_split_field_raw = config.get("split_field", split_field)
        effective_split_field = str(effective_split_field_raw) if effective_split_field_raw else split_field
        
        if suffix in (".yaml", ".yml"):
            with open(resolved_path, encoding="utf-8") as f:
                data = yaml.safe_load(f)
        elif suffix == ".json":
            import json
            with open(resolved_path, encoding="utf-8") as f:
                data = json.load(f)
        elif suffix == ".csv":
            import csv
            samples: list[Sample] = []
            with open(resolved_path, newline="", encoding="utf-8") as f:
                reader = csv.DictReader(f)
                for row in reader:
                    split_str = row.get(effective_split_field) or "dev"
                    try:
                        split = Split(split_str.lower())
                    except ValueError:
                        split = Split("dev")
                    samples.append(
                        Sample(
                            id=row.get("id", f"sample_{len(samples)}") or f"sample_{len(samples)}",
                            input=row.get("input", "") or "",
                            expected=row.get("expected", "") or "",
                            split=split,
                        )
                    )
            return samples
        else:
            raise ValueError(f"Unsupported file format: {suffix}")
        
        # Parse the loaded data with the effective split field
        effective_split_str: str = str(effective_split_field)
        return self._parse_data_with_split_field(data, effective_split_str)
    
    def _parse_data(self, data: dict[str, object]) -> list[Sample]:
        """Parse samples from loaded data structure.
        
        Expected formats:
            - {"samples": [{"id": "...", "input": "...", "expected": "...", "split": "..."}]}
            - [{"id": "...", "input": "...", "expected": "...", "split": "..."}]
            - {"name": "...", "path": "..."} (Dataset config format - loads referenced file)
        """
        if isinstance(data, list):
            sample_dicts = data
        elif isinstance(data, dict):
            # Check if this is a Dataset config format (name, path, split_field)
            if "name" in data and "path" in data and "samples" not in data:
                # Dataset config format - load the referenced data file
                return self._load_dataset_config(data, self.split_field)
            
            sample_dicts = data.get("samples", [])
            if not isinstance(sample_dicts, list):
                raise ValueError("Data must contain a 'samples' list or be a list itself")
        else:
            raise ValueError(f"Unexpected data type: {type(data)}")
        
        samples: list[Sample] = []
        
        for sample_dict in sample_dicts:
            if not isinstance(sample_dict, dict):
                raise ValueError(f"Sample must be a dict, got: {type(sample_dict)}")
            
            id_value = sample_dict.get("id")
            if not id_value or not isinstance(id_value, str):
                id_str = f"sample_{len(samples)}"
            else:
                id_str = id_value
            
            input_value = sample_dict.get("input")
            input_str = str(input_value) if input_value is not None else ""
            
            expected_val = sample_dict.get("expected", "")
            
            split_value = sample_dict.get(self.split_field, "dev")
            if isinstance(split_value, str):
                try:
                    split = Split(split_value.lower())
                except ValueError:
                    split = Split("dev")
            else:
                split = Split("dev")
            
            samples.append(
                Sample(
                    id=str(id_str),
                    input=str(input_str),
                    expected=expected_val,
                    split=split,
                )
            )
        
        return samples
    
    def _parse_data_with_split_field(self, data: dict[str, object], split_field: str) -> list[Sample]:
        """Parse samples with a custom split field name."""
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
            
            id_value = sample_dict.get("id")
            if not id_value or not isinstance(id_value, str):
                id_str = f"sample_{len(samples)}"
            else:
                id_str = id_value
            
            input_value = sample_dict.get("input")
            input_str = str(input_value) if input_value is not None else ""
            
            expected_val = sample_dict.get("expected", "")
            
            split_value = sample_dict.get(split_field, "dev")
            if isinstance(split_value, str):
                try:
                    split = Split(split_value.lower())
                except ValueError:
                    split = Split("dev")
            else:
                split = Split("dev")
            
            samples.append(
                Sample(
                    id=str(id_str),
                    input=str(input_str),
                    expected=expected_val,
                    split=split,
                )
            )
        
        return samples
