import React from 'react';
import PropTypes from 'prop-types';

import Button from 'src/widget/buttons/button';

const AcceptButton = (props) => {
    return (
        <Button
            emphasis={'secondary'}
            onClick={() => props.accept(props.issueId)}
        >
            {'내 목록에 추가'}
        </Button>
    );
};

AcceptButton.propTypes = {
    issueId: PropTypes.string.isRequired,
    accept: PropTypes.func.isRequired,
};

export default AcceptButton;
