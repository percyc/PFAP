#include "utils.tcc"
#include "note.tcc"
#include "comparison.tcc"
#include "sub_cmp.tcc"
#include "commitment.tcc"
#include "merkle.tcc"

/***********************************************************
 * Modified redeem circuit
 ***********************************************************
 * publicData: sn_A_old, rt_cmt, cmt_A_new, value_s
 * privateData: sk_A, r_A_old, value_old, r_A_new, value_new, sn_A_new, path
 ***********************************************************
 * Constraints:
 *   sn_A_new = SHA256(sk_A, sn_A_old)
 *   value_new = value_old - value_s
 *   value_s < value_old
 *   cmt_A_old = sha256_CMTA(value_old, sn_A_old, r_A_old)
 *   cmt_A_new = sha256_CMTA(value_new, sn_A_new, r_A_new)
 *   path is Merkle Tree path from cmt_A_old to rt_cmt
 **********************************************************/
template<typename FieldT>
class redeem_gadget : public gadget<FieldT> {
public:
    pb_variable_array<FieldT> zk_packed_inputs;
    pb_variable_array<FieldT> zk_unpacked_inputs;
    std::shared_ptr<multipacking_gadget<FieldT>> unpacker;

    std::shared_ptr<digest_variable<FieldT>> zk_merkle_root;
    pb_variable<FieldT> value_enforce;
    std::shared_ptr<merkle_tree_gadget<FieldT>> witness_input;

    pb_variable_array<FieldT> value;
    pb_variable_array<FieldT> value_old;
    pb_variable_array<FieldT> value_s;
    std::shared_ptr<digest_variable<FieldT>> sk;
    std::shared_ptr<digest_variable<FieldT>> r;
    std::shared_ptr<digest_variable<FieldT>> r_old;

    std::shared_ptr<note_gadget_with_comparison_and_subtraction_for_value_old<FieldT>> ncsv;

    std::shared_ptr<digest_variable<FieldT>> sn;
    std::shared_ptr<sha256_PRF_gadget<FieldT>> prf_to_inputs_sn;

    std::shared_ptr<digest_variable<FieldT>> sn_old;

    std::shared_ptr<digest_variable<FieldT>> cmtA_old;
    std::shared_ptr<sha256_CMTA_gadget<FieldT>> commit_to_inputs_cmt_old;

    std::shared_ptr<digest_variable<FieldT>> cmtA;
    std::shared_ptr<sha256_CMTA_gadget<FieldT>> commit_to_inputs_cmt;

    pb_variable<FieldT> ZERO;

    redeem_gadget(
        protoboard<FieldT>& pb
    ) : gadget<FieldT>(pb) {
        {
            zk_packed_inputs.allocate(pb, verifying_field_element_size());
            this->pb.set_input_sizes(verifying_field_element_size());

            alloc_uint256(zk_unpacked_inputs, sn_old);
            alloc_uint256(zk_unpacked_inputs, zk_merkle_root);
            alloc_uint256(zk_unpacked_inputs, cmtA);

            alloc_uint64(zk_unpacked_inputs, this->value_s);

            assert(zk_unpacked_inputs.size() == verifying_input_bit_size());

            unpacker.reset(new multipacking_gadget<FieldT>(
                pb,
                zk_unpacked_inputs,
                zk_packed_inputs,
                FieldT::capacity(),
                "unpacker"
            ));
        }

        value_enforce.allocate(pb);

        ZERO.allocate(this->pb, FMT(this->annotation_prefix, "zero"));

        value.allocate(pb, 64);
        value_old.allocate(pb, 64);
        sk.reset(new digest_variable<FieldT>(pb, 256, "private key"));

        r.reset(new digest_variable<FieldT>(pb, 256, "random number"));
        r_old.reset(new digest_variable<FieldT>(pb, 256, "old random number"));
        sn.reset(new digest_variable<FieldT>(pb, 256, "serial number"));

        ncsv.reset(new note_gadget_with_comparison_and_subtraction_for_value_old<FieldT>(
            pb,
            value,
            value_old,
            value_s,
            sk,
            r,
            r_old,
            sn,
            sn_old
        ));

        prf_to_inputs_sn.reset(new sha256_PRF_gadget<FieldT>(
            pb,
            ZERO,
            sk->bits,
            sn_old->bits,
            sn
        ));

        cmtA_old.reset(new digest_variable<FieldT>(pb, 256, "cmtA_old"));

        commit_to_inputs_cmt_old.reset(new sha256_CMTA_gadget<FieldT>(
            pb,
            ZERO,
            value_old,
            sn_old->bits,
            r_old->bits,
            cmtA_old
        ));

        commit_to_inputs_cmt.reset(new sha256_CMTA_gadget<FieldT>(
            pb,
            ZERO,
            value,
            sn->bits,
            r->bits,
            cmtA
        ));

        witness_input.reset(new merkle_tree_gadget<FieldT>(
            pb,
            *cmtA_old,
            *zk_merkle_root,
            value_enforce
        ));
    }

    void generate_r1cs_constraints() {
        unpacker->generate_r1cs_constraints(true);

        ncsv->generate_r1cs_constraints();

        generate_r1cs_equals_const_constraint<FieldT>(this->pb, ZERO, FieldT::zero(), "ZERO");

        sn->generate_r1cs_constraints();
        prf_to_inputs_sn->generate_r1cs_constraints();

        sn_old->generate_r1cs_constraints();

        cmtA_old->generate_r1cs_constraints();
        commit_to_inputs_cmt_old->generate_r1cs_constraints();

        cmtA->generate_r1cs_constraints();
        commit_to_inputs_cmt->generate_r1cs_constraints();

        zk_merkle_root->generate_r1cs_constraints();
        generate_boolean_r1cs_constraint<FieldT>(this->pb, value_enforce, "");
        witness_input->generate_r1cs_constraints();
    }

    void generate_r1cs_witness(
        const Note& note_old,
        const Note& note,
        uint256 cmtA_old_data,
        uint256 cmtA_data,
        uint64_t v_s,
        uint256 sk_data,
        const uint256& rt,
        const MerklePath& path
    ) {
        ncsv->generate_r1cs_witness(note_old, note, v_s, sk_data);

        this->pb.val(value_enforce) = FieldT::one();

        this->pb.val(ZERO) = FieldT::zero();

        // Fill PRF inputs and output FIRST, then call witness generation
        sn_old->bits.fill_with_bits(
            this->pb,
            uint256_to_bool_vector(note_old.sn)
        );
        sn->bits.fill_with_bits(
            this->pb,
            uint256_to_bool_vector(note.sn)
        );

        prf_to_inputs_sn->generate_r1cs_witness();

        commit_to_inputs_cmt_old->generate_r1cs_witness();
        commit_to_inputs_cmt->generate_r1cs_witness();

        cmtA_old->bits.fill_with_bits(
            this->pb,
            uint256_to_bool_vector(cmtA_old_data)
        );
        cmtA->bits.fill_with_bits(
            this->pb,
            uint256_to_bool_vector(cmtA_data)
        );

        witness_input->generate_r1cs_witness(path);

        zk_merkle_root->bits.fill_with_bits(
            this->pb,
            uint256_to_bool_vector(rt)
        );

        unpacker->generate_r1cs_witness_from_bits();
    }

    static r1cs_primary_input<FieldT> witness_map(
        const uint256& sn_old,
        const uint256& rt,
        const uint256& cmtA,
        uint64_t value_s
    ) {
        std::vector<bool> verify_inputs;

        insert_uint256(verify_inputs, sn_old);
        insert_uint256(verify_inputs, rt);
        insert_uint256(verify_inputs, cmtA);

        insert_uint64(verify_inputs, value_s);

        assert(verify_inputs.size() == verifying_input_bit_size());
        auto verify_field_elements = pack_bit_vector_into_field_element_vector<FieldT>(verify_inputs);
        assert(verify_field_elements.size() == verifying_field_element_size());
        return verify_field_elements;
    }

    static size_t verifying_input_bit_size() {
        size_t acc = 0;

        acc += 256; // sn_old
        acc += 256; // rt_cmt (merkle root)
        acc += 256; // cmtA

        acc += 64; // value_s

        return acc;
    }

    static size_t verifying_field_element_size() {
        return div_ceil(verifying_input_bit_size(), FieldT::capacity());
    }

    void alloc_uint256(
        pb_variable_array<FieldT>& packed_into,
        std::shared_ptr<digest_variable<FieldT>>& var
    ) {
        var.reset(new digest_variable<FieldT>(this->pb, 256, ""));
        packed_into.insert(packed_into.end(), var->bits.begin(), var->bits.end());
    }

    void alloc_uint64(
        pb_variable_array<FieldT>& packed_into,
        pb_variable_array<FieldT>& integer
    ) {
        integer.allocate(this->pb, 64, "");
        packed_into.insert(packed_into.end(), integer.begin(), integer.end());
    }
};
