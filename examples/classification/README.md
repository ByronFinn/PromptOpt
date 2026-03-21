# Classification Example

这是一个用于标签分类任务的 PromptOpt 示例项目。

## 目录结构

```text
classification/
├── tasks/task.yaml
├── candidates/baseline.yaml
├── datasets/dataset.yaml
├── datasets/intent.json
└── .promptopt.yaml
```

## 快速开始

```bash
promptopt eval \
  --task tasks/task.yaml \
  --candidate candidates/baseline.yaml \
  --dataset datasets/dataset.yaml \
  --split dev
```
