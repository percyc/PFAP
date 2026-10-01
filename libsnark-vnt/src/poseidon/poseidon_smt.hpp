#ifndef POSEIDON_SMT_HPP_
#define POSEIDON_SMT_HPP_

#include <memory>
#include <vector>
#include <string>
#include "poseidon.hpp"
#include "uint256.h"

// Global, public, depth-256 sparse Merkle tree (the "state Merkle Tree").
//
//  * Depth = 256. Leaves are indexed by a 256-bit key.
//  * A leaf's value is 0 (never inserted) or 1 (inserted). A leaf is updated at
//    most once, from 0 to 1.
//  * The key of cmt is path = Poseidon(cmt), taken as the canonical 256-bit
//    big-endian bit string of the field element. path_bit[i] (i = 0 is the MSB)
//    selects the branch at tree level i (level 0 just below the root); the leaf
//    sits at level 256.
//  * Node hashing uses Poseidon 2-to-1: parent = Poseidon(left, right).
//  * Empty subtree roots are precomputed (empty_root[d]) so that proofs and the
//    root can be produced without materializing 2^256 nodes.
//
// Immutable path-copying nodes share unchanged subtrees between snapshots.
// Cloning a tree copies its root pointer, not all historical commitments.

namespace poseidon {

static const size_t SMT_DEPTH = 256;

class PoseidonSMT {
public:
    PoseidonSMT() {
        // empty_root[0] = empty leaf = 0.
        // empty_root[d] = Poseidon(empty_root[d-1], empty_root[d-1]).
        empty_root_.resize(SMT_DEPTH + 1);
        empty_root_[0] = FieldT::zero();
        for (size_t d = 1; d <= SMT_DEPTH; d++) {
            empty_root_[d] = poseidon_hash2(empty_root_[d-1], empty_root_[d-1]);
        }
    }

    // The current root (height SMT_DEPTH).
    FieldT root() const {
        return value(root_, SMT_DEPTH);
    }

    uint256 root_uint256() const {
        return field_to_uint256_be(root());
    }

    // Compute the 256-bit path for a commitment: path = Poseidon(cmt).
    // path_bits[0] is the MSB (top branch), path_bits[255] is the LSB (leaf).
    static std::vector<bool> compute_path_bits(const uint256& cmt) {
        FieldT c = field_from_cmt(cmt);
        FieldT p = poseidon_hash1(c);
        return field_to_path_bits(p);
    }

    static FieldT compute_path_field(const uint256& cmt) {
        FieldT c = field_from_cmt(cmt);
        return poseidon_hash1(c);
    }

    // Insert a commitment: set its leaf to 1 and update ancestors to the root.
    void insert(const uint256& cmt) {
        std::vector<bool> bits = compute_path_bits(cmt);
        root_ = insert_path(root_, bits, 0);
    }

    // Membership proof for a commitment that has been inserted.
    // Returns siblings[0..255] (sibling at each level, from leaf upward:
    // siblings[0] is the sibling of the leaf, siblings[255] is the sibling just
    // below the root) and path_bits (MSB-first as in compute_path_bits).
    struct Proof {
        std::vector<bool> path_bits;        // length 256, MSB-first
        std::vector<uint256> siblings;      // length 256, leaf-first
        uint256 root;
        uint256 path;                       // Poseidon(cmt) as uint256
        bool found;
    };

    Proof prove(const uint256& cmt) const {
        Proof pr;
        std::vector<bool> bits = compute_path_bits(cmt);
        pr.path_bits = bits;
        pr.path = uint256_from_field(compute_path_field(cmt));
        pr.siblings.resize(SMT_DEPTH);
        auto node = root_;
        for (size_t depth = 0; depth < SMT_DEPTH; depth++) {
            const size_t height = SMT_DEPTH - depth - 1;
            auto sibling = node ? (bits[depth] ? node->left : node->right) : nullptr;
            pr.siblings[height] = uint256_from_field(value(sibling, height));
            node = node ? (bits[depth] ? node->right : node->left) : nullptr;
        }
        pr.found = node && node->hash == FieldT::one();
        pr.root = root_uint256();
        return pr;
    }

    // Convert a field element to its canonical 256-bit big-endian bit vector.
    static std::vector<bool> field_to_path_bits(const FieldT& f) {
        libff::bigint<libff::alt_bn128_r_limbs> b = f.as_bigint();
        std::vector<bool> bits(256, false);
        // b has 4 limbs little-endian; build big-endian bit string (bit 0 = MSB).
        for (size_t i = 0; i < 256; i++) {
            size_t bitpos = 255 - i;           // little-endian bit position
            size_t limb = bitpos / 64;
            size_t off = bitpos % 64;
            bool v = false;
            if (limb < 4) v = ((b.data[limb] >> off) & 1ULL) != 0;
            bits[i] = v;
        }
        return bits;
    }

private:
    struct Node;
    using NodePtr = std::shared_ptr<const Node>;
    struct Node {
        FieldT hash;
        NodePtr left, right;
        Node(const FieldT& h, NodePtr l = nullptr, NodePtr r = nullptr)
            : hash(h), left(std::move(l)), right(std::move(r)) {}
    };
    std::vector<FieldT> empty_root_;
    NodePtr root_;

    FieldT value(const NodePtr& node, size_t height) const {
        return node ? node->hash : empty_root_[height];
    }

    NodePtr insert_path(const NodePtr& node, const std::vector<bool>& bits, size_t depth) const {
        if (depth == SMT_DEPTH) {
            return node ? node : std::make_shared<Node>(FieldT::one());
        }
        auto left = node ? node->left : nullptr;
        auto right = node ? node->right : nullptr;
        if (bits[depth]) right = insert_path(right, bits, depth + 1);
        else left = insert_path(left, bits, depth + 1);
        // Insertion is idempotent, including across shared historical snapshots.
        if (node && left == node->left && right == node->right) return node;
        const size_t height = SMT_DEPTH - depth - 1;
        return std::make_shared<Node>(poseidon_hash2(value(left, height), value(right, height)), left, right);
    }
};

} // namespace poseidon

#endif // POSEIDON_SMT_HPP_
