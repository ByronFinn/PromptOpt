# JSON Extraction Example

这是一个使用 PromptOpt 进行 Prompt 优化的示例项目。

## 目录结构

```
json_extraction/
├── tasks/
│   └── task.yaml          # 任务定义
├── candidates/
│   └── baseline.yaml      # Baseline prompt
├── datasets/
│   └── dataset.yaml       # 数据集配置
└── .promptopt.yaml        # 本地配置
```

## 快速开始

1. 初始化项目：
```bash
promptopt init json_extraction
```

2. 运行 baseline 评估：
```bash
promptopt eval \
  --task tasks/task.yaml \
  --candidate candidates/baseline.yaml \
  --dataset datasets/dataset.yaml \
  --split dev
```

3. 分析失败案例：
```bash
promptopt diagnose runs/run_001
```

4. 生成优化候选：
```bash
promptopt optimize runs/run_001 \
  --teacher openai/gpt-4 \
  --strategies rewrite \
  --num-candidates 12
```

5. 批量评估：
```bash
promptopt search candidates/ \
  --task tasks/task.yaml \
  --dataset datasets/dataset.yaml \
  --split dev
```

6. 选择最优：
```bash
promptopt select runs/run_002 \
  --primary macro_f1 \
  --constraints json_validity=1.0
```

7. 测试集验证：
```bash
promptopt verify runs/run_002 --split test
```
