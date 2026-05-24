template<typename FieldT>
class merkle_tree_gadget : gadget<FieldT> {
private:
    typedef sha256_two_to_one_hash_gadget<FieldT> sha256_gadget; // sha256(left, right)

    pb_variable_array<FieldT> positions; // depth bits of the current node
    std::shared_ptr<merkle_authentication_path_variable<FieldT, sha256_gadget>> authvars; // Merkle authentication path variable
    std::shared_ptr<merkle_tree_check_read_gadget<FieldT, sha256_gadget>> auth; // Merkle path checking gadget

public:
    // Constructor
    merkle_tree_gadget(
        protoboard<FieldT>& pb,
        digest_variable<FieldT> leaf, // leaf hash
        digest_variable<FieldT> root, // root hash
        pb_variable<FieldT>& enforce
    ) : gadget<FieldT>(pb) {
        positions.allocate(pb, INCREMENTAL_MERKLE_TREE_DEPTH);
        authvars.reset(new merkle_authentication_path_variable<FieldT, sha256_gadget>(
            pb, INCREMENTAL_MERKLE_TREE_DEPTH, "auth"
        ));
        auth.reset(new merkle_tree_check_read_gadget<FieldT, sha256_gadget>(
            pb,
            INCREMENTAL_MERKLE_TREE_DEPTH, // tree height
            positions,  // address_bits - depth bits of the current node, size must equal tree height
            leaf,
            root,
            *authvars,
            enforce, // read_successful, do_copy
            ""
        ));
    }

    // Generate constraints for the private variables of merkle_tree_gadget
    void generate_r1cs_constraints() {
        for (size_t i = 0; i < INCREMENTAL_MERKLE_TREE_DEPTH; i++) {
            // TODO: This might not be necessary, and doesn't
            // appear to be done in libsnark's tests, but there
            // is no documentation, so let's do it anyway to
            // be safe.
            generate_boolean_r1cs_constraint<FieldT>( // add a boolean constraint for each position bit
                this->pb,
                positions[i],
                "boolean_positions"
            );
        }

        authvars->generate_r1cs_constraints();
        auth->generate_r1cs_constraints();
    }

    // Generate witness for the private variables of merkle_tree_gadget
    void generate_r1cs_witness(const MerklePath& path) {
        // TODO: Change libsnark so that it doesn't require this goofy
        // number thing in its API.
        size_t path_index = convertVectorToInt(path.index);

        positions.fill_with_bits_of_ulong(this->pb, path_index);

        authvars->generate_r1cs_witness(path_index, path.authentication_path);
        auth->generate_r1cs_witness();
    }
};
