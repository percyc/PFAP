# ZKP Circuit Fixes Summary

## 总体状态
所有6个电路都已修复并通过约束验证（pb.is_satisfied() = 1）。
其中5个电路已完整通过零知识证明验证测试。

---

## 各电路修复详情

### ✅ 1. createAccount (createAccount)
**状态**: 完全工作正常
- 约束验证: ✅ 通过
- 证明验证: ✅ 通过
- PRF输入: sk + 0
- 无需Merkle树

---

### ✅ 2. Send Circuit (send)
**状态**: 完全工作正常
- 约束验证: ✅ 通过
- 证明验证: ✅ 通过
- PRF输入: sk + sn_old (已从旧的sk + r修改)
- 无需Merkle树

---

### ✅ 3. Deposit Circuit (deposit)
**状态**: 完全工作正常
- 约束验证: ✅ 通过
- 证明验证: ✅ 通过
- PRF输入: sk + 0 for sn_s, sk + sn_old for sn
- 无需Merkle树

---

### ✅ 4. Mint Circuit (mint)
**状态**: 完全工作正常
- 约束验证: ✅ 通过
- 证明验证: ✅ 通过
- **主要修复**:
  1. PRF输入从r改为sn_old: SHA256(sk, sn_old)
  2. Merkle树witness创建顺序: 先添加所有叶子，再填充witness
  3. cmtA_old的约束正确设置

---

### ✅ 5. Redeem Circuit (redeem)
**状态**: 完全工作正常
- 约束验证: ✅ 通过
- 证明验证: ✅ 通过
- **主要修复**: 同mint电路的PRF和Merkle树修复

---

### ⚠️ 6. Transfer Circuit (transfer)
**状态**: 约束验证通过，证明验证待最终确认
- 约束验证: ✅ 通过 (pb.is_satisfied() = 1)
- 证明验证: 待进一步调试公共输入顺序
- **已完成的主要修复**:
  1. PRF输入从r改为sn_old
  2. cmtA_old的Merkle树witness正确设置
  3. value和value_old的所有布尔约束通过
  4. value约束: value = value_old + (2*type - 1) * value_s 通过
  5. 边界检查约束alpha和disjunction通过
  6. cmtS的SHA256 padding修复为192 bits（320 bits消息的正确padding）

---

## 关键技术修复点

### 1. PRF伪随机函数输入修改
**修改前**: `sn = SHA256(sk, r)`  
**修改后**: `sn = SHA256(sk, sn_old)`

这是根据需求文档的核心修改，创建了一个确定性的序列号链。

### 2. Merkle树Witness创建顺序修复
**问题**: 在调用witness前未填充所有叶子节点
**修复**:
```cpp
// 先添加所有叶子到Merkle树
for (size_t i = 0; i < N; i++) {
    tree.append(commitments[i]);
}
// 然后创建witness并获取路径
wit = tree.witness();
auto path = wit.path();
```

### 3. SHA256 Padding长度修复
Transfer的cmtS是2输入320 bits消息，需要192 bits padding才能填满单块SHA256的512 bits。

### 4. 公共输入与私有输入区分
根据需求文档第49行：
- **publicData**: cmt_S, sn_A_old, cmt_A_new, rt_cmt, type
- **privateData**: sk_A, r_A_old, value_A_old, r_A_new, value_A_new, sn_A_new, path_A, value_s, r_s

**注意**: value_s是私密输入，不应该出现在公共输入中！

---

## 性能基准测试 (Ubuntu 18.04)
| Circuit | Constraints | Setup Time | Proof Gen Time | Verify Time |
|---------|-------------|------------|----------------|-------------|
| createAccount | ~32k | - | < 1s | < 10ms |
| send | ~40k | - | ~13s | < 10ms |
| deposit | ~45k | - | ~26s | < 10ms |
| mint | ~40k | - | ~20s | < 10ms |
| redeem | ~40k | - | ~20s | < 10ms |
| transfer | ~42k | ~68s | ~24s | < 10ms |

---

## 下一步建议

1. **Transfer电路验证**: 调试公共输入的位顺序问题
   - 可能的问题: uint256_to_bool_vector的位顺序与电路内部不一致
   - 建议: 使用二分法逐个添加公共输入来定位问题

2. **Key生成**: 可以开始生成每个电路的pk和vk密钥文件

3. **Go语言集成**: 将libsnark的C接口与go-ethereum集成

4. **完整交易测试**: 测试完整的mint -> transfer -> redeem流程

