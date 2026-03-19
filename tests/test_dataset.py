"""Tests for DatasetLoader."""

from pathlib import Path

import pytest

from promptopt.core import DatasetLoader, Sample, Split


class TestDatasetLoader:
    """Tests for DatasetLoader class."""
    
    DATA_DIR = Path(__file__).parent / "data"
    
    def test_load_json(self) -> None:
        """Test loading JSON dataset."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.json"))
        samples = loader.load()
        
        assert len(samples) == 3
        assert samples[0].id == "sample_001"
        assert samples[0].split == Split.DEV
        assert "咳嗽" in samples[0].input
    
    def test_load_yaml(self) -> None:
        """Test loading YAML dataset."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.yaml"))
        samples = loader.load()
        
        assert len(samples) == 3
        assert samples[1].id == "sample_002"
        assert samples[1].split == Split.TEST
    
    def test_load_csv(self) -> None:
        """Test loading CSV dataset."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.csv"))
        samples = loader.load()
        
        assert len(samples) == 3
        assert samples[2].id == "sample_003"
        assert samples[2].split == Split.TRAIN
    
    def test_load_json_list_format(self) -> None:
        """Test loading JSON dataset with list format (no samples key)."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset_list.json"))
        samples = loader.load()
        
        assert len(samples) == 2
        assert samples[0].id == "sample_001"
    
    def test_load_with_split_filter_dev(self) -> None:
        """Test loading with dev split filter."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.json"))
        samples = loader.load(split=Split.DEV)
        
        assert len(samples) == 1
        assert samples[0].id == "sample_001"
        assert samples[0].split == Split.DEV
    
    def test_load_with_split_filter_test(self) -> None:
        """Test loading with test split filter."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.json"))
        samples = loader.load(split=Split.TEST)
        
        assert len(samples) == 1
        assert samples[0].id == "sample_002"
        assert samples[0].split == Split.TEST
    
    def test_load_with_split_filter_train(self) -> None:
        """Test loading with train split filter."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.json"))
        samples = loader.load(split=Split.TRAIN)
        
        assert len(samples) == 1
        assert samples[0].id == "sample_003"
        assert samples[0].split == Split.TRAIN
    
    def test_load_file_not_found(self) -> None:
        """Test that FileNotFoundError is raised for missing file."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "nonexistent.json"))
        
        with pytest.raises(FileNotFoundError):
            loader.load()
    
    def test_load_unsupported_format(self, tmp_path: Path) -> None:
        """Test that ValueError is raised for unsupported format."""
        test_file = tmp_path / "dataset.txt"
        test_file.write_text("dummy content")
        loader = DatasetLoader(path=str(test_file))
        
        with pytest.raises(ValueError, match="Unsupported file format"):
            loader.load()
    
    def test_sample_fields(self) -> None:
        """Test that Sample has all required fields."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.json"))
        samples = loader.load()
        
        sample = samples[0]
        assert isinstance(sample, Sample)
        assert sample.id == "sample_001"
        assert sample.input == "患者男性，58岁，因反复咳嗽、咳痰伴喘息10年，加重1周入院。"
        assert isinstance(sample.expected, dict)
        assert sample.split == Split.DEV
    
    def test_sample_expected_dict(self) -> None:
        """Test that expected field can be a dict."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset.json"))
        samples = loader.load()
        
        sample = samples[0]
        assert isinstance(sample.expected, dict)
        assert sample.expected["疾病"] == "慢性支气管炎"
    
    def test_load_dataset_config_format(self) -> None:
        """Test loading Dataset config format (name, path, split_field)."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset_config.yaml"))
        samples = loader.load()
        
        assert len(samples) == 3
        assert samples[0].id == "sample_001"
        assert samples[0].split == Split.DEV
        assert "咳嗽" in samples[0].input
    
    def test_load_dataset_config_with_split_filter(self) -> None:
        """Test loading Dataset config with split filter."""
        loader = DatasetLoader(path=str(self.DATA_DIR / "dataset_config.yaml"))
        samples = loader.load(split=Split.TEST)
        
        assert len(samples) == 1
        assert samples[0].id == "sample_002"
        assert samples[0].split == Split.TEST


class TestSample:
    """Tests for Sample dataclass."""
    
    def test_sample_creation(self) -> None:
        """Test creating a Sample instance."""
        sample = Sample(
            id="test_001",
            input="Test input",
            expected={"key": "value"},
            split=Split.DEV,
        )
        
        assert sample.id == "test_001"
        assert sample.input == "Test input"
        assert sample.expected == {"key": "value"}
        assert sample.split == Split.DEV
    
    def test_sample_string_expected(self) -> None:
        """Test that expected can be a string."""
        sample = Sample(
            id="test_002",
            input="Test input",
            expected="expected output",
            split=Split.TEST,
        )
        
        assert sample.expected == "expected output"
    
    def test_sample_slots(self) -> None:
        """Test that Sample uses __slots__."""
        sample = Sample(
            id="test_003",
            input="input",
            expected="output",
            split=Split.TRAIN,
        )
        
        with pytest.raises(AttributeError):
            sample.nonexistent_attr = "value"
