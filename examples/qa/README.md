# QA Example

这是一个用于单轮问答任务的 PromptOpt 示例项目。

## 目录结构

```text
qa/
├── tasks/task.yaml
├── candidates/baseline.yaml
├── datasets/dataset.yaml
├── datasets/qa.json
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
