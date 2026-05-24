#include "utils.tcc"
#include "comparison.tcc"
#include "commitment.tcc"
#include "merkle.tcc"

/***********************************************************
 * Transfer circuit
 ***********************************************************
 * publicData: cmt_S, sn_A_old, cmt_A_new, rt_cmt, type
 * privateData: sk_A, r_A_old, value_A_old, r_A_new, value_A_new, sn_A_new, path_A, value_s, r_s
 ***********************************************************
 * Constraints:
 *   cmt_S = sha256_CMTS(value_s, r_s)
 *   sn_A_new = SHA256(sk_A, sn_A_old)
 *   value_A_new = value_A_old + (2*type - 1) * value_s
 *   (1-type) * (value_A_old - value_s) >= 0
 *   cmt_A_old = sha256_CMTA(value_A_old, sn_A_old, r_A_old)
 *   cmt_A_new = sha256_CMTA(value_A_new, sn_A_new, r_A_new)
 *   path_A is Merkle Tree path from cmt_A_old to rt_cmt
 **********************************************************/
template<typename FieldT>
class transfer_gadget : public gadget<FieldT> {
public:
    pb_variable_array<FieldT> zk_packed_inputs;
    pb_variable_array<FieldT> zk_unpacked_inputs;
    std::shared_ptr<multipacking_gadget<FieldT>> unpacker;

    std::shared_ptr<digest_variable<FieldT>> zk_merkle_root;
    pb_variable<FieldT> value_enforce;
    std::shared_ptr<merkle_tree_gadget<FieldT>> witness_input;

    pb_variable_array<FieldT> value_s;
    pb_variable_array<FieldT> value_old;
    std::shared_ptr<digest_variable<FieldT>> r_s;

    std::shared_ptr<digest_variable<FieldT>> sk;
    std::shared_ptr<digest_variable<FieldT>> sn_old;
    std::shared_ptr<digest_variable<FieldT>> r_old;

    pb_variable_array<FieldT> value;
    std::shared_ptr<digest_variable<FieldT>> sn;
    std::shared_ptr<digest_variable<FieldT>> r_new;

    pb_variable<FieldT> type_packed;
    pb_variable_array<FieldT> type_bits;

    pb_variable<FieldT> value_packed;
    pb_variable<FieldT> value_old_packed;
    pb_variable<FieldT> value_s_packed;

    pb_variable<FieldT> delta;
    pb_variable<FieldT> effective_diff;

    pb_variable_array<FieldT> alpha;
    pb_variable<FieldT> alpha_packed;
    std::shared_ptr<packing_gadget<FieldT>> pack_alpha;
    std::shared_ptr<disjunction_gadget<FieldT>> all_zeros_test;
    pb_variable<FieldT> not_all_zeros;

    std::shared_ptr<sha256_PRF_gadget<FieldT>> prf_to_inputs_sn;

    std::shared_ptr<digest_variable<FieldT>> cmtS;
    std::shared_ptr<sha256_CMTS_transfer_gadget<FieldT>> commit_to_input_cmt_s;

    std::shared_ptr<digest_variable<FieldT>> cmtA_old;
    std::shared_ptr<sha256_CMTA_gadget<FieldT>> commit_to_inputs_cmt_old;

    std::shared_ptr<digest_variable<FieldT>> cmtA;
    std::shared_ptr<sha256_CMTA_gadget<FieldT>> commit_to_inputs_cmt;

    pb_variable<FieldT> ZERO;

    transfer_gadget(
        protoboard<FieldT>& pb
    ) : gadget<FieldT>(pb) {
        {
            zk_packed_inputs.allocate(pb, verifying_field_element_size());
            this->pb.set_input_sizes(verifying_field_element_size());

            alloc_uint256(zk_unpacked_inputs, cmtS);
            alloc_uint256(zk_unpacked_inputs, sn_old);
            alloc_uint256(zk_unpacked_inputs, cmtA);
            alloc_uint256(zk_unpacked_inputs, zk_merkle_root);
            alloc_uint1(zk_unpacked_inputs, this->type_bits);

            assert(zk_unpacked_inputs.size() == verifying_input_bit_size());

            unpacker.reset(new multipacking_gadget<FieldT>(
                pb, zk_unpacked_inputs, zk_packed_inputs,
                FieldT::capacity(), "unpacker"
            ));
        }

        value_enforce.allocate(pb);
        ZERO.allocate(this->pb, FMT(this->annotation_prefix, "zero"));

        // Private inputs
        value_s.allocate(pb, 64);
        value_old.allocate(pb, 64, "value_old");
        value.allocate(pb, 64);

        r_s.reset(new digest_variable<FieldT>(pb, 256, "r_s"));
        sk.reset(new digest_variable<FieldT>(pb, 256, "private key"));
        r_old.reset(new digest_variable<FieldT>(pb, 256, "old random number"));
        sn.reset(new digest_variable<FieldT>(pb, 256, "serial number"));
        r_new.reset(new digest_variable<FieldT>(pb, 256, "new random number"));

        cmtA_old.reset(new digest_variable<FieldT>(pb, 256, "cmtA_old"));

        // Packed values
        value_packed.allocate(pb, "value_packed");
        value_old_packed.allocate(pb, "value_old_packed");
        value_s_packed.allocate(pb, "value_s_packed");
        type_packed.allocate(pb, "type_packed");
        delta.allocate(pb, "delta");
        effective_diff.allocate(pb, "effective_diff");

        // Alpha for overflow check
        alpha.allocate(pb, 65, "alpha");
        alpha_packed.allocate(pb, "alpha_packed");
        not_all_zeros.allocate(pb, "not_all_zeros");

        // Gadgets
        pack_alpha.reset(new packing_gadget<FieldT>(pb, alpha, alpha_packed, "pack_alpha"));
        all_zeros_test.reset(new disjunction_gadget<FieldT>(pb,
            pb_variable_array<FieldT>(alpha.begin(), alpha.begin() + 64),
            not_all_zeros, "all_zeros_test"));

        prf_to_inputs_sn.reset(new sha256_PRF_gadget<FieldT>(
            pb, ZERO, sk->bits, sn_old->bits, sn
        ));

        commit_to_input_cmt_s.reset(new sha256_CMTS_transfer_gadget<FieldT>(
            pb, ZERO, value_s, r_s->bits, cmtS
        ));

        commit_to_inputs_cmt_old.reset(new sha256_CMTA_gadget<FieldT>(
            pb, ZERO, value_old, sn_old->bits, r_old->bits, cmtA_old
        ));

        commit_to_inputs_cmt.reset(new sha256_CMTA_gadget<FieldT>(
            pb, ZERO, value, sn->bits, r_new->bits, cmtA
        ));

        witness_input.reset(new merkle_tree_gadget<FieldT>(
            pb, *cmtA_old, *zk_merkle_root, value_enforce
        ));
    }

    void generate_r1cs_constraints() {
        unpacker->generate_r1cs_constraints(true);

        generate_r1cs_equals_const_constraint<FieldT>(this->pb, ZERO, FieldT::zero(), "ZERO");

        for (size_t i = 0; i < 64; i++) {
            generate_boolean_r1cs_constraint<FieldT>(this->pb, value_s[i], "boolean_value_s");
        }
        for (size_t i = 0; i < 64; i++) {
            generate_boolean_r1cs_constraint<FieldT>(this->pb, value[i], "boolean_value");
        }
        for (size_t i = 0; i < 64; i++) {
            generate_boolean_r1cs_constraint<FieldT>(this->pb, value_old[i], "boolean_value_old");
        }

        generate_boolean_r1cs_constraint<FieldT>(this->pb, type_bits[0], "boolean_type");

        r_s->generate_r1cs_constraints();
        sk->generate_r1cs_constraints();
        r_old->generate_r1cs_constraints();
        sn->generate_r1cs_constraints();
        r_new->generate_r1cs_constraints();
        sn_old->generate_r1cs_constraints();

        prf_to_inputs_sn->generate_r1cs_constraints();

        cmtS->generate_r1cs_constraints();
        commit_to_input_cmt_s->generate_r1cs_constraints();

        cmtA_old->generate_r1cs_constraints();
        r_old->generate_r1cs_constraints();
        commit_to_inputs_cmt_old->generate_r1cs_constraints();

        cmtA->generate_r1cs_constraints();
        r_new->generate_r1cs_constraints();
        sn->generate_r1cs_constraints();
        commit_to_inputs_cmt->generate_r1cs_constraints();

        zk_merkle_root->generate_r1cs_constraints();
        generate_boolean_r1cs_constraint<FieldT>(this->pb, value_enforce, "");
        witness_input->generate_r1cs_constraints();

        this->pb.add_r1cs_constraint(r1cs_constraint<FieldT>(
            type_packed, value_s_packed, delta
        ), "delta = type * value_s");

        this->pb.add_r1cs_constraint(r1cs_constraint<FieldT>(
            1, (value_old_packed - value_s_packed + 2 * delta), value_packed
        ), "value = value_old + (2*type - 1) * value_s");

        this->pb.add_r1cs_constraint(r1cs_constraint<FieldT>(
            (1 - type_packed), value_s_packed, effective_diff
        ), "effective_diff = (1-type) * value_s");

        pack_alpha->generate_r1cs_constraints(true);
        this->pb.add_r1cs_constraint(r1cs_constraint<FieldT>(
            1, (FieldT(2)^64) + value_old_packed - effective_diff, alpha_packed
        ), "alpha = 2^64 + value_old - effective_diff");

        // (1-type) * (1 - alpha[64]) = 0
        // forces value_old >= value_s when type=0 (payer)
        this->pb.add_r1cs_constraint(r1cs_constraint<FieldT>(
            FieldT::one() - type_packed, FieldT::one() - alpha[64], FieldT::zero()
        ), "value_old >= value_s");
    }

    void generate_r1cs_witness(
        const Note& note_old,
        const Note& note,
        uint64_t v_s,
        uint256 r_s_data,
        uint256 sk_data,
        uint256 cmtS_data,
        uint256 cmtA_old_data,
        uint256 cmtA_data,
        const uint256& rt,
        const MerklePath& path,
        uint8_t type_val
    ) {
        this->pb.val(ZERO) = FieldT::zero();

        this->pb.val(value_enforce) = FieldT::one();

        value_s.fill_with_bits(this->pb, uint64_to_bool_vector(v_s));
        this->pb.lc_val(value_s_packed) = value_s.get_field_element_from_bits_by_order(this->pb);

        value.fill_with_bits(this->pb, uint64_to_bool_vector(note.value));
        this->pb.lc_val(value_packed) = value.get_field_element_from_bits_by_order(this->pb);

        value_old.fill_with_bits(this->pb, uint64_to_bool_vector(note_old.value));
        this->pb.lc_val(value_old_packed) = value_old.get_field_element_from_bits_by_order(this->pb);

        this->pb.val(type_bits[0]) = type_val ? FieldT::one() : FieldT::zero();
        this->pb.val(type_packed) = type_val ? FieldT::one() : FieldT::zero();

        // delta = type * value_s
        this->pb.val(delta) = (this->pb.val(type_packed) * this->pb.lc_val(value_s_packed));
        // effective_diff = (1-type) * value_s
        this->pb.val(effective_diff) = (FieldT::one() - this->pb.val(type_packed)) * this->pb.lc_val(value_s_packed);
        // alpha witness
        this->pb.val(alpha_packed) = (FieldT(2)^64) + this->pb.lc_val(value_old_packed) - this->pb.val(effective_diff);
        pack_alpha->generate_r1cs_witness_from_packed();
        all_zeros_test->generate_r1cs_witness();

        // Fill PRF inputs and output first
        sn_old->bits.fill_with_bits(this->pb, uint256_to_bool_vector(note_old.sn));
        sn->bits.fill_with_bits(this->pb, uint256_to_bool_vector(note.sn));
        sk->bits.fill_with_bits(this->pb, uint256_to_bool_vector(sk_data));
        r_s->bits.fill_with_bits(this->pb, uint256_to_bool_vector(r_s_data));
        r_old->bits.fill_with_bits(this->pb, uint256_to_bool_vector(note_old.r));
        r_new->bits.fill_with_bits(this->pb, uint256_to_bool_vector(note.r));

        prf_to_inputs_sn->generate_r1cs_witness();

        // call witness generation to fill SHA256 intermediate variables
        commit_to_input_cmt_s->generate_r1cs_witness();
        commit_to_inputs_cmt_old->generate_r1cs_witness();
        commit_to_inputs_cmt->generate_r1cs_witness();

        // Fill public inputs manually (matching mint's pattern)
        cmtS->bits.fill_with_bits(this->pb, uint256_to_bool_vector(cmtS_data));
        cmtA_old->bits.fill_with_bits(this->pb, uint256_to_bool_vector(cmtA_old_data));
        cmtA->bits.fill_with_bits(this->pb, uint256_to_bool_vector(cmtA_data));

        witness_input->generate_r1cs_witness(path);
        zk_merkle_root->bits.fill_with_bits(this->pb, uint256_to_bool_vector(rt));
        this->pb.val(value_enforce) = FieldT::one();

        unpacker->generate_r1cs_witness_from_bits();
    }

    static r1cs_primary_input<FieldT> witness_map(
        const uint256& cmtS,
        const uint256& sn_old,
        const uint256& cmtA,
        const uint256& rt,
        bool type
    ) {
        std::vector<bool> verify_inputs;

        insert_uint256(verify_inputs, cmtS);
        insert_uint256(verify_inputs, sn_old);
        insert_uint256(verify_inputs, cmtA);
        insert_uint256(verify_inputs, rt);
        verify_inputs.push_back(type);

        assert(verify_inputs.size() == verifying_input_bit_size());
        auto verify_field_elements = pack_bit_vector_into_field_element_vector<FieldT>(verify_inputs);
        assert(verify_field_elements.size() == verifying_field_element_size());
        return verify_field_elements;
    }

    static size_t verifying_input_bit_size() {
        size_t acc = 0;

        acc += 256; // cmtS
        acc += 256; // sn_old
        acc += 256; // cmtA
        acc += 256; // rt_cmt (merkle root)
        acc += 1;   // type

        return acc;
    }

    static size_t verifying_field_element_size() {
        return div_ceil(verifying_input_bit_size(), FieldT::capacity());
    }

    void alloc_uint256(pb_variable_array<FieldT>& packed_into,
                        std::shared_ptr<digest_variable<FieldT>>& var) {
        var.reset(new digest_variable<FieldT>(this->pb, 256, ""));
        packed_into.insert(packed_into.end(), var->bits.begin(), var->bits.end());
    }

    void alloc_uint64(pb_variable_array<FieldT>& packed_into,
                       pb_variable_array<FieldT>& integer) {
        integer.allocate(this->pb, 64, "");
        packed_into.insert(packed_into.end(), integer.begin(), integer.end());
    }

    void alloc_uint1(pb_variable_array<FieldT>& packed_into,
                      pb_variable_array<FieldT>& bits) {
        bits.allocate(this->pb, 1, "");
        packed_into.insert(packed_into.end(), bits.begin(), bits.end());
    }
};
